// Package store holds the domain logic of workload running state.
//
// K8s API access is confined to internal/k8s and used here via the ScaledObjectLister /
// StateRepository interfaces. ClusterStore owns a single cluster; MultiStore orchestrates many.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
	targets   k8s.ScaleTargetReader // nil: newly discovered workloads start as stopped
	metrics   metrics.MetricsManager
	workloads map[string]*model.Workload // key: "namespace/name"
}

// ClusterStoreOption customizes a ClusterStore.
type ClusterStoreOption func(*ClusterStore)

// WithScaleTargetReader makes newly discovered workloads start as running when their scale target has replicas.
func WithScaleTargetReader(r k8s.ScaleTargetReader) ClusterStoreOption {
	return func(s *ClusterStore) { s.targets = r }
}

// NewClusterStore creates a ClusterStore.
func NewClusterStore(c config.Cluster, scaledObj k8s.ScaledObjectLister, state k8s.StateRepository, m metrics.MetricsManager, opts ...ClusterStoreOption) *ClusterStore {
	s := &ClusterStore{
		cluster:   c,
		scaledObj: scaledObj,
		state:     state,
		metrics:   m,
		workloads: make(map[string]*model.Workload),
	}
	for _, o := range opts {
		o(s)
	}
	return s
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

	return true
}

// StopAll stops the workloads allow accepts (nil = all). It returns the number stopped and the
// "namespace/name" of running workloads that were skipped.
func (s *ClusterStore) StopAll(_ context.Context, allow func(namespace, workload string) bool) (int, []string, error) {
	s.mu.Lock()
	stopped := 0
	var skipped []string
	type meta struct{ ns, name string }
	var changed []meta
	for _, w := range s.workloads {
		if allow != nil && !allow(w.Namespace, w.Name) {
			if w.Replicas > 0 {
				skipped = append(skipped, w.Namespace+"/"+w.Name)
			}
			continue
		}
		if w.Replicas > 0 {
			stopped++
		}
		w.Replicas = 0
		w.StartedBy = ""
		changed = append(changed, meta{ns: w.Namespace, name: w.Name})
	}
	slices.Sort(skipped)
	entries := s.snapshotEntriesLocked()
	s.mu.Unlock()

	s.persist(entries)
	for _, mw := range changed {
		s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, mw.ns, mw.name, 0)
	}

	return stopped, skipped, nil
}

// snapshot returns copies of all workloads.
func (s *ClusterStore) snapshot() []model.Workload {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Workload, 0, len(s.workloads))
	for _, w := range s.workloads {
		out = append(out, *w)
	}
	return out
}

// Bootstrap is the one-time startup initialization.
// It lists ScaledObjects to build the registry, then restores state from the ConfigMap.
func (s *ClusterStore) Bootstrap(ctx context.Context) error {
	entries, err := s.state.Load(ctx)
	loaded := err == nil
	if err != nil {
		slog.Warn("failed to load workload state", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "load-state"))
		entries = nil
	}

	registered, err := s.sync(ctx, entries)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.restoreLocked(entries)
	snapshot := s.snapshotEntriesLocked()
	s.mu.Unlock()

	// Persist only on a successful load, otherwise the saved state would be overwritten.
	if registered && loaded {
		s.persist(snapshot)
	}
	return nil
}

// Reconcile lists ScaledObjects and refreshes the registry.
func (s *ClusterStore) Reconcile(ctx context.Context) error {
	registered, err := s.sync(ctx, nil)
	if err != nil {
		return err
	}
	if registered {
		s.mu.RLock()
		snapshot := s.snapshotEntriesLocked()
		s.mu.RUnlock()
		s.persist(snapshot)
	}
	return nil
}

// sync refreshes the registry from the ScaledObject list. known keys (already in the state
// ConfigMap) skip the scale target read. It reports whether a running workload was registered.
func (s *ClusterStore) sync(ctx context.Context, known map[string]k8s.StateEntry) (bool, error) {
	items, err := s.scaledObj.List(ctx)
	if err != nil {
		return false, fmt.Errorf("list ScaledObjects (cluster=%s): %w", s.cluster.Name, err)
	}

	seen := make(map[string]struct{}, len(items))
	for i := range items {
		so := &items[i]
		if !hasActivationTrigger(so) {
			continue
		}
		seen[so.Namespace+"/"+so.Spec.ScaleTargetRef.Name] = struct{}{}
	}

	initial := s.readInitialReplicas(ctx, items, known)

	s.mu.Lock()
	defer s.mu.Unlock()

	registered := false
	for i := range items {
		if s.handleScaledObjectLocked(&items[i], initial) {
			registered = true
		}
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

	return registered, nil
}

// readInitialReplicas reads the scale target of each newly discovered workload (no lock held).
// Workloads whose target cannot be read are absent from the result and start as stopped.
func (s *ClusterStore) readInitialReplicas(ctx context.Context, items []kedav1alpha1.ScaledObject, known map[string]k8s.StateEntry) map[string]int {
	out := map[string]int{}
	if s.targets == nil {
		return out
	}
	s.mu.RLock()
	registered := make(map[string]struct{}, len(s.workloads))
	for key := range s.workloads {
		registered[key] = struct{}{}
	}
	s.mu.RUnlock()

	for i := range items {
		so := &items[i]
		if !hasActivationTrigger(so) {
			continue
		}
		name := so.Spec.ScaleTargetRef.Name
		key := so.Namespace + "/" + name
		if _, ok := registered[key]; ok {
			continue
		}
		if _, ok := known[key]; ok {
			continue
		}
		if _, ok := out[key]; ok {
			continue
		}
		kind := scaleTargetKind(so)
		replicas, err := s.targets.Replicas(ctx, kind, so.Namespace, name)
		if err != nil {
			slog.Warn("failed to read scale target, registering as stopped", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", so.Namespace), slog.String("kind", kind), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "read-scale-target"))
			continue
		}
		out[key] = int(replicas)
	}
	return out
}

func scaleTargetKind(so *kedav1alpha1.ScaledObject) string {
	if k := so.Spec.ScaleTargetRef.Kind; k != "" {
		return k
	}
	return "Deployment"
}

// handleScaledObjectLocked registers/updates/removes a ScaledObject in the registry. Requires mu.
// initial holds the current replicas of new targets. It reports whether a running workload was registered.
func (s *ClusterStore) handleScaledObjectLocked(so *kedav1alpha1.ScaledObject, initial map[string]int) bool {
	ns := so.Namespace
	name := so.Spec.ScaleTargetRef.Name
	kind := scaleTargetKind(so)
	key := ns + "/" + name

	if !hasActivationTrigger(so) {
		if _, ok := s.workloads[key]; ok {
			delete(s.workloads, key)
			s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, ns, name, 0)
			s.metrics.DeleteMetric(s.cluster.Env, s.cluster.Name, ns, name)
			slog.Debug("ScaledObject unregistered (trigger removed)", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", ns))
		}
		return false
	}

	if existing, ok := s.workloads[key]; ok {
		existing.Kind = kind
		return false
	}

	replicas := 0
	var activation float64
	if initial[key] > 0 {
		replicas = 1
		activation = 1
	}
	s.workloads[key] = &model.Workload{
		Env:       s.cluster.Env,
		Name:      name,
		Namespace: ns,
		Kind:      kind,
		Replicas:  replicas,
	}
	s.metrics.SetActivation(s.cluster.Env, s.cluster.Name, ns, name, activation)

	slog.Debug("ScaledObject registered", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", ns), slog.String("kind", kind), slog.Int("replicas", replicas))
	return replicas > 0
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

// restoreLocked applies the loaded state entries to the registry. Requires mu.
func (s *ClusterStore) restoreLocked(entries map[string]k8s.StateEntry) {
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
