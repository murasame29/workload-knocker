// Package store holds the domain logic of workload running state.
//
// Workload discovery and actuation go through the actuator.Discoverer / actuator.Actuator interfaces
// and state persistence through k8s.StateRepository. ClusterStore owns a single cluster; MultiStore
// orchestrates many.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/notify"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// ClusterStore manages workload state of a single managed cluster.
// MultiStore aggregates them to form model.WorkloadStore.
//
// Locking:
//   - mu protects read/write of the workloads map (short-lived); never held across K8s API calls.
//   - persistMu serializes state.Save; acquire it after releasing mu when calling from Start/Stop.
type ClusterStore struct {
	mu         sync.RWMutex
	persistMu  sync.Mutex
	cluster    config.Cluster
	discoverer actuator.Discoverer
	actuator   actuator.Actuator
	state      k8s.StateRepository
	now        func() time.Time
	workloads  map[string]*model.Workload // key: "namespace/name"
}

// NewClusterStore creates a ClusterStore.
func NewClusterStore(c config.Cluster, d actuator.Discoverer, a actuator.Actuator, state k8s.StateRepository) *ClusterStore {
	return &ClusterStore{
		cluster:    c,
		discoverer: d,
		actuator:   a,
		state:      state,
		now:        time.Now,
		workloads:  make(map[string]*model.Workload),
	}
}

// ref returns the actuator identity of w.
func ref(w *model.Workload) actuator.Workload {
	return actuator.Workload{Namespace: w.Namespace, Name: w.Name, Kind: w.Kind}
}

// apply sends the desired state of w to the actuator. A refusal is returned as *model.RefusedError;
// other failures are logged and the in-memory state stays, so a later reconcile retries.
func (s *ClusterStore) apply(w actuator.Workload, running bool) error {
	err := s.actuator.Apply(context.Background(), w, running)
	if r, ok := actuator.AsRefusal(err); ok {
		return &model.RefusedError{Reason: r.Error()}
	}
	if err != nil {
		slog.Warn("failed to apply workload state", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", w.Name), slog.String("namespace", w.Namespace), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "apply-state"))
	}
	return nil
}

func (s *ClusterStore) forget(w actuator.Workload) {
	if err := s.actuator.Forget(context.Background(), w); err != nil {
		slog.Warn("failed to forget workload", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", w.Name), slog.String("namespace", w.Namespace), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "forget-state"))
	}
}

// effect is an actuator call collected under mu and run after it is released.
type effect struct {
	w       actuator.Workload
	running bool
	forget  bool
}

func (s *ClusterStore) run(effects []effect) {
	for _, e := range effects {
		// The actuator reports its own refusals (for example the GitOps conflict gauge).
		_ = s.apply(e.w, e.running)
		if e.forget {
			s.forget(e.w)
		}
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

// WorkloadsByNamespace returns copies of the workloads of the given namespace.
func (s *ClusterStore) WorkloadsByNamespace(ns string) []*model.Workload {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*model.Workload
	for _, w := range s.workloads {
		if w.Namespace == ns {
			c := *w
			result = append(result, &c)
		}
	}
	return result
}

// Start sets the workload to running: in-memory update, then persist, then metrics.
// Persisting (a K8s API call) runs under persistMu after releasing mu.
// A refusal from the actuator puts the previous state back.
func (s *ClusterStore) Start(ns, name, user string, stopAt time.Time) error {
	return s.change(ns+"/"+name, true, runState{replicas: 1, startedBy: user, startedAt: s.now(), stopAt: stopAt})
}

// runState is the part of a workload that Start and Stop change.
type runState struct {
	replicas  int
	startedBy string
	startedAt time.Time
	stopAt    time.Time
}

func stateOf(w *model.Workload) runState {
	return runState{replicas: w.Replicas, startedBy: w.StartedBy, startedAt: w.StartedAt, stopAt: w.StopAt}
}

func (r runState) setOn(w *model.Workload) {
	w.Replicas, w.StartedBy, w.StartedAt, w.StopAt = r.replicas, r.startedBy, r.startedAt, r.stopAt
}

// change sets the run state of key, persists it and applies it; a refusal restores the previous
// state unless another operation changed it meanwhile.
func (s *ClusterStore) change(key string, running bool, next runState) error {
	s.mu.Lock()
	w, ok := s.workloads[key]
	if !ok {
		s.mu.Unlock()
		return model.ErrWorkloadNotFound
	}
	prev := stateOf(w)
	next.setOn(w)
	entries := s.snapshotEntriesLocked()
	target := ref(w)
	s.mu.Unlock()

	s.persist(entries)
	err := s.apply(target, running)
	if err == nil {
		return nil
	}

	s.mu.Lock()
	if w, ok := s.workloads[key]; ok && stateOf(w) == next {
		prev.setOn(w)
	}
	entries = s.snapshotEntriesLocked()
	s.mu.Unlock()
	s.persist(entries)
	return err
}

// SetStopAt moves the auto-stop of a running workload.
func (s *ClusterStore) SetStopAt(ns, name string, stopAt time.Time) bool {
	s.mu.Lock()
	w, ok := s.workloads[ns+"/"+name]
	if !ok || w.Replicas == 0 {
		s.mu.Unlock()
		return false
	}
	w.StopAt = stopAt
	entries := s.snapshotEntriesLocked()
	s.mu.Unlock()

	s.persist(entries)
	return true
}

// Stop sets the workload to stopped.
// A refusal from the actuator puts the previous state back.
func (s *ClusterStore) Stop(ns, name string) error {
	return s.change(ns+"/"+name, false, runState{})
}

// StopAll stops the workloads allow accepts (nil = all). It returns the number stopped and the
// "namespace/name" of running workloads that were skipped.
func (s *ClusterStore) StopAll(_ context.Context, allow func(namespace, workload string) bool) (int, []string, error) {
	s.mu.Lock()
	stopped := 0
	var skipped []string
	var changed []effect
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
		w.StartedAt = time.Time{}
		w.StopAt = time.Time{}
		changed = append(changed, effect{w: ref(w)})
	}
	slices.Sort(skipped)
	entries := s.snapshotEntriesLocked()
	s.mu.Unlock()

	s.persist(entries)
	s.run(changed)

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
// It discovers workloads to build the registry, then restores state from the ConfigMap.
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
	restored := s.restoreLocked(entries)
	snapshot := s.snapshotEntriesLocked()
	s.mu.Unlock()
	s.run(restored)
	slog.Info("workload state restored", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env))

	// Persist only on a successful load, otherwise the saved state would be overwritten.
	if registered && loaded {
		s.persist(snapshot)
	}
	return nil
}

// Reconcile re-discovers workloads and refreshes the registry.
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

// sync refreshes the registry from the discovered workloads. known keys (already in the state
// ConfigMap) skip the replicas read. It reports whether a running workload was registered.
func (s *ClusterStore) sync(ctx context.Context, known map[string]k8s.StateEntry) (bool, error) {
	items, err := s.discoverer.Discover(ctx)
	if err != nil {
		return false, fmt.Errorf("discover workloads (cluster=%s): %w", s.cluster.Name, err)
	}

	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		seen[it.Namespace+"/"+it.Name] = struct{}{}
	}

	initial := s.readInitialReplicas(ctx, items, known)

	var effects []effect
	s.mu.Lock()
	registered := false
	for _, it := range items {
		if s.handleLocked(it, initial, &effects) {
			registered = true
		}
	}

	for key, w := range s.workloads {
		if _, ok := seen[key]; ok {
			continue
		}
		delete(s.workloads, key)
		effects = append(effects, effect{w: ref(w), forget: true})
		slog.Debug("ScaledObject removed (reconcile)", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", w.Name), slog.String("namespace", w.Namespace))
	}
	s.mu.Unlock()

	s.run(effects)
	return registered, nil
}

// readInitialReplicas reads the replicas of each newly discovered workload (no lock held).
// Workloads whose replicas cannot be read are absent from the result and start as stopped.
func (s *ClusterStore) readInitialReplicas(ctx context.Context, items []actuator.Workload, known map[string]k8s.StateEntry) map[string]int {
	out := map[string]int{}
	s.mu.RLock()
	registered := make(map[string]struct{}, len(s.workloads))
	for key := range s.workloads {
		registered[key] = struct{}{}
	}
	s.mu.RUnlock()

	for _, it := range items {
		key := it.Namespace + "/" + it.Name
		if _, ok := registered[key]; ok {
			continue
		}
		if _, ok := known[key]; ok {
			continue
		}
		if _, ok := out[key]; ok {
			continue
		}
		replicas, err := s.discoverer.Replicas(ctx, it)
		if errors.Is(err, actuator.ErrReplicasUnknown) {
			return out
		}
		if err != nil {
			slog.Warn("failed to read scale target, registering as stopped", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", it.Name), slog.String("namespace", it.Namespace), slog.String("kind", it.Kind), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "read-scale-target"))
			continue
		}
		out[key] = int(replicas)
	}
	return out
}

// handleLocked registers or updates a discovered workload in the registry. Requires mu.
// initial holds the current replicas of new targets. It reports whether a running workload was registered.
func (s *ClusterStore) handleLocked(it actuator.Workload, initial map[string]int, effects *[]effect) bool {
	ns, name, kind := it.Namespace, it.Name, it.Kind
	key := ns + "/" + name

	def, maxDur, problems := runlimit.FromAnnotations(it.Annotations)
	if len(problems) > 0 {
		slog.Warn("invalid run duration annotation ignored", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", ns), slog.Any("annotations", problems))
	}

	if existing, ok := s.workloads[key]; ok {
		existing.Kind = kind
		existing.DefaultDuration, existing.MaxDuration = def, maxDur
		s.setReadyMessage(existing, it.Annotations, false)
		return false
	}

	replicas := 0
	if initial[key] > 0 {
		replicas = 1
	}
	w := &model.Workload{
		Env:       s.cluster.Env,
		Name:      name,
		Namespace: ns,
		Kind:      kind,
		Replicas:  replicas,

		DefaultDuration: def,
		MaxDuration:     maxDur,
	}
	s.workloads[key] = w
	s.setReadyMessage(w, it.Annotations, true)
	*effects = append(*effects, effect{w: ref(w), running: replicas > 0})

	slog.Debug("ScaledObject registered", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", name), slog.String("namespace", ns), slog.String("kind", kind), slog.Int("replicas", replicas))
	return replicas > 0
}

// setReadyMessage copies the ready-message annotation to w and warns once per changed invalid template.
func (s *ClusterStore) setReadyMessage(w *model.Workload, annotations map[string]string, discovered bool) {
	tmpl := annotations[notify.ReadyMessageAnnotation]
	if !discovered && tmpl == w.ReadyMessage {
		return
	}
	w.ReadyMessage = tmpl
	if tmpl == "" {
		return
	}
	if err := notify.ValidateReady(tmpl); err != nil {
		slog.Warn("invalid ready-message template, the plain ready line is used", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("workload", w.Name), slog.String("namespace", w.Namespace), slog.String("annotation", notify.ReadyMessageAnnotation), slog.String("error.message", err.Error()))
	}
}

// restoreLocked applies the loaded state entries to the registry and returns the actuator calls
// that publish the restored state. Requires mu.
func (s *ClusterStore) restoreLocked(entries map[string]k8s.StateEntry) []effect {
	for key, entry := range entries {
		w, ok := s.workloads[key]
		if !ok {
			// Ignore entries absent from the registry
			continue
		}
		w.Replicas = entry.Replicas
		w.StartedBy = entry.StartedBy
		w.StartedAt = entry.StartedAt
		w.StopAt = entry.StopAt
	}

	effects := make([]effect, 0, len(s.workloads))
	for _, w := range s.workloads {
		effects = append(effects, effect{w: ref(w), running: w.Replicas > 0})
	}
	return effects
}

// snapshotEntriesLocked returns a snapshot of the in-memory state. Requires mu (read or write).
// The caller passes it to persist after releasing mu.
func (s *ClusterStore) snapshotEntriesLocked() map[string]k8s.StateEntry {
	entries := make(map[string]k8s.StateEntry, len(s.workloads))
	for key, w := range s.workloads {
		entries[key] = k8s.StateEntry{
			Replicas:  w.Replicas,
			StartedBy: w.StartedBy,
			StartedAt: w.StartedAt,
			StopAt:    w.StopAt,
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
