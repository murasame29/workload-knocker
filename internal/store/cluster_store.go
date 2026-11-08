// Package store holds the domain logic of workload running state.
//
// K8s API access is confined to internal/k8s and used here via the ScaledObjectLister /
// StateRepository interfaces. ClusterStore owns a single cluster; MultiStore orchestrates many.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

// activationTriggerMetric is the metric name that Prometheus trigger queries managed by
// workload-knocker should contain.
const activationTriggerMetric = "workload_knocker_activation"

// ClusterStore manages workload state of a single managed cluster.
// MultiStore aggregates them to form model.WorkloadStore.
//
// Locking:
//   - mu protects read/write of the workloads map (short-lived); never held across K8s API calls.
//   - persistMu serializes state.Save; acquire it after releasing mu when calling from Start/Stop.
type ClusterStore struct {
	mu        sync.RWMutex
	persistMu sync.Mutex
	cluster   config.Cluster
	scaledObj k8s.ScaledObjectLister
	state     k8s.StateRepository
	metrics   metrics.MetricsManager
	workloads map[string]*model.Workload // key: "namespace/name"
}

// NewClusterStore creates a ClusterStore.
func NewClusterStore(c config.Cluster, scaledObj k8s.ScaledObjectLister, state k8s.StateRepository, m metrics.MetricsManager) *ClusterStore {
	return &ClusterStore{
		cluster:   c,
		scaledObj: scaledObj,
		state:     state,
		metrics:   m,
		workloads: make(map[string]*model.Workload),
	}
}

// Namespaces returns all namespaces in the registry.
func (s *ClusterStore) Namespaces() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[string]bool)
	var ns []string
	for _, w := range s.workloads {
		if !seen[w.Namespace] {
			seen[w.Namespace] = true
			ns = append(ns, w.Namespace)
		}
	}
	return ns
}

// WorkloadsByNamespace returns the workloads of the given namespace.
func (s *ClusterStore) WorkloadsByNamespace(ns string) []*model.Workload {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*model.Workload
	for _, w := range s.workloads {
		if w.Namespace == ns {
			result = append(result, w)
		}
	}
	return result
}

// Start sets the workload to running: in-memory update, then persist, then metrics.
// Persisting (a K8s API call) runs under persistMu after releasing mu.
func (s *ClusterStore) Start(ns, name, user string) bool {
	s.mu.Lock()
	w, ok := s.workloads[ns+"/"+name]
	if !ok {
		s.mu.Unlock()
		return false
	}
	w.Replicas = 1
	w.StartedBy = user
	entries := s.snapshotEntriesLocked()
	s.mu.Unlock()

	s.persist(entries)
	s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, ns, name, 1)

	slog.Info("workload started", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", ns), slog.String("action", "start"), slog.String("user.name", user))
	return true
}

// Stop sets the workload to stopped.
func (s *ClusterStore) Stop(ns, name string) bool {
	s.mu.Lock()
	w, ok := s.workloads[ns+"/"+name]
	if !ok {
		s.mu.Unlock()
		return false
	}
	w.Replicas = 0
	w.StartedBy = ""
	entries := s.snapshotEntriesLocked()
	s.mu.Unlock()

	s.persist(entries)
	s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, ns, name, 0)

	slog.Info("workload stopped", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", ns), slog.String("action", "stop"))
	return true
}

// StopAll stops all workloads. Returns the number of workloads running at call time.
func (s *ClusterStore) StopAll(_ context.Context) (int, error) {
	s.mu.Lock()
	stopped := 0
	for _, w := range s.workloads {
		if w.Replicas > 0 {
			stopped++
		}
		w.Replicas = 0
		w.StartedBy = ""
	}
	entries := s.snapshotEntriesLocked()
	// Collect the namespace/name list for metrics
	type meta struct{ ns, name string }
	metaList := make([]meta, 0, len(s.workloads))
	for _, w := range s.workloads {
		metaList = append(metaList, meta{ns: w.Namespace, name: w.Name})
	}
	count := len(s.workloads)
	s.mu.Unlock()

	s.persist(entries)
	for _, mw := range metaList {
		s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, mw.ns, mw.name, 0)
	}

	slog.Info("all workloads stopped", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("action", "stop-all"), slog.Int("count", count), slog.Int("stopped", stopped))
	return stopped, nil
}

// Bootstrap is the one-time startup initialization.
// It lists ScaledObjects to build the registry, then restores state from the ConfigMap.
func (s *ClusterStore) Bootstrap(ctx context.Context) error {
	if err := s.reconcile(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreLocked(ctx)
	return nil
}

// Reconcile lists ScaledObjects and refreshes the registry.
func (s *ClusterStore) Reconcile(ctx context.Context) error {
	return s.reconcile(ctx)
}

func (s *ClusterStore) reconcile(ctx context.Context) error {
	items, err := s.scaledObj.List(ctx)
	if err != nil {
		return fmt.Errorf("list ScaledObjects (cluster=%s): %w", s.cluster.Name, err)
	}

	seen := make(map[string]struct{}, len(items))
	for i := range items {
		so := &items[i]
		if !hasActivationTrigger(so) {
			continue
		}
		seen[so.Namespace+"/"+so.Spec.ScaleTargetRef.Name] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range items {
		s.handleScaledObjectLocked(&items[i])
	}

	for key, w := range s.workloads {
		if _, ok := seen[key]; ok {
			continue
		}
		delete(s.workloads, key)
		s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, w.Namespace, w.Name, 0)
		s.metrics.DeleteMetric(s.cluster.Env, s.cluster.Name, w.Namespace, w.Name)
		slog.Debug("ScaledObject removed (reconcile)", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", w.Name), slog.String("namespace", w.Namespace))
	}

	return nil
}

// handleScaledObjectLocked registers/updates/removes a ScaledObject in the registry. Requires mu.
func (s *ClusterStore) handleScaledObjectLocked(so *kedav1alpha1.ScaledObject) {
	ns := so.Namespace
	name := so.Spec.ScaleTargetRef.Name
	kind := so.Spec.ScaleTargetRef.Kind
	if kind == "" {
		kind = "Deployment"
	}
	key := ns + "/" + name

	if !hasActivationTrigger(so) {
		if _, ok := s.workloads[key]; ok {
			delete(s.workloads, key)
			s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, ns, name, 0)
			s.metrics.DeleteMetric(s.cluster.Env, s.cluster.Name, ns, name)
			slog.Debug("ScaledObject unregistered (trigger removed)", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", ns))
		}
		return
	}

	if existing, ok := s.workloads[key]; ok {
		existing.Kind = kind
		return
	}

	s.workloads[key] = &model.Workload{
		Env:       s.cluster.Env,
		Name:      name,
		Namespace: ns,
		Kind:      kind,
		Replicas:  0,
	}
	s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, ns, name, 0)

	slog.Debug("ScaledObject registered", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", ns), slog.String("kind", kind))
}

func hasActivationTrigger(so *kedav1alpha1.ScaledObject) bool {
	for _, trigger := range so.Spec.Triggers {
		if trigger.Type != "prometheus" {
			continue
		}
		query, ok := trigger.Metadata["query"]
		if ok && strings.Contains(query, activationTriggerMetric) {
			return true
		}
	}
	return false
}

// restoreLocked restores state from the state repository. Requires mu.
func (s *ClusterStore) restoreLocked(ctx context.Context) {
	entries, err := s.state.Load(ctx)
	if err != nil {
		slog.Warn("failed to load workload state", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "load-state"))
		return
	}

	for key, entry := range entries {
		w, ok := s.workloads[key]
		if !ok {
			// Ignore entries absent from the registry
			continue
		}
		w.Replicas = entry.Replicas
		w.StartedBy = entry.StartedBy
	}

	for _, w := range s.workloads {
		var val float64
		if w.Replicas > 0 {
			val = 1
		}
		s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, w.Namespace, w.Name, val)
	}

	slog.Info("workload state restored", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env))
}

// snapshotEntriesLocked returns a snapshot of the in-memory state. Requires mu (read or write).
// The caller passes it to persist after releasing mu.
func (s *ClusterStore) snapshotEntriesLocked() map[string]k8s.StateEntry {
	entries := make(map[string]k8s.StateEntry, len(s.workloads))
	for key, w := range s.workloads {
		entries[key] = k8s.StateEntry{
			Replicas:  w.Replicas,
			StartedBy: w.StartedBy,
		}
	}
	return entries
}

// persist saves the snapshot to the state repository. Must not hold mu.
// Serializing with persistMu avoids conflicting Save updates.
func (s *ClusterStore) persist(entries map[string]k8s.StateEntry) {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	err := s.state.Save(context.Background(), entries)
	if err != nil {
		slog.Warn("failed to save workload state", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "save-state"))
	}
}
