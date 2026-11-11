package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/rest"

	"github.com/murasame29/workload-knocker/internal/actuator/factory"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

// RestConfigSource obtains a *rest.Config from a config.Cluster.
// k8sauth.Multiplexer satisfies it in production; tests can inject a fake.
type RestConfigSource interface {
	RestConfig(ctx context.Context, c config.Cluster) (*rest.Config, error)
}

// MultiStore aggregates all ClusterStores in *config.Config and implements model.WorkloadStore.
type MultiStore struct {
	// mu guards registry and stores; both are replaced wholesale by Reload, never mutated in place.
	mu             sync.RWMutex
	reloadMu       sync.Mutex
	registry       *config.Config
	stores         map[string]*ClusterStore // key = Cluster.Env
	metrics        metrics.MetricsManager
	stopAllTimeout time.Duration
}

// snapshot returns the registry and store set at one instant.
func (m *MultiStore) snapshot() (*config.Config, map[string]*ClusterStore) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.registry, m.stores
}

func (m *MultiStore) storeFor(env string) (*ClusterStore, bool) {
	_, stores := m.snapshot()
	cs, ok := stores[env]
	return cs, ok
}

// Compile-time interface check.
var _ model.WorkloadStore = (*MultiStore)(nil)

// NewMultiStore eagerly builds a ClusterStore for each registered cluster.
// stopAllTimeout is the per-cluster timeout of StopAll.
// It builds the clients from the *rest.Config of RestConfigSource and injects the resulting
// StateRepository and the cluster's actuator into the ClusterStore.
// A client creation failure only logs a warning and skips that cluster; error only if all fail.
func NewMultiStore(ctx context.Context, reg *config.Config, src RestConfigSource, m metrics.MetricsManager, stopAllTimeout time.Duration) (*MultiStore, error) {
	build := NewClusterFactory(src, m)
	stores := make(map[string]*ClusterStore)
	for _, c := range reg.All() {
		cs, err := build(ctx, c, reg.Controller.Namespace)
		if err != nil {
			slog.Warn("failed to build cluster store", slog.String("cluster.env", c.Env), slog.String("cluster.name", c.Name), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "build-cluster-store"))
			m.SetReachable(c.Name, 0)
			continue
		}
		stores[c.Env] = cs
	}
	if len(stores) == 0 {
		return nil, fmt.Errorf("multi-store: no managed cluster could be initialized (all clients failed)")
	}
	return &MultiStore{
		registry:       reg,
		stores:         stores,
		metrics:        m,
		stopAllTimeout: stopAllTimeout,
	}, nil
}

// ClusterFactory builds an un-bootstrapped ClusterStore for one cluster.
type ClusterFactory func(ctx context.Context, c config.Cluster, controllerNs string) (*ClusterStore, error)

// NewClusterFactory returns the production factory (rest.Config from src, real K8s clients,
// the actuator selected by the cluster).
func NewClusterFactory(src RestConfigSource, m metrics.MetricsManager) ClusterFactory {
	return func(ctx context.Context, c config.Cluster, controllerNs string) (*ClusterStore, error) {
		restCfg, err := src.RestConfig(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("build rest.Config: %w", err)
		}
		repo, err := k8s.NewStateRepositoryForConfig(restCfg, controllerNs, c.Env)
		if err != nil {
			return nil, fmt.Errorf("build state repository: %w", err)
		}
		reservations, err := k8s.NewReservationStoreForConfig(restCfg, controllerNs, c.Env)
		if err != nil {
			return nil, fmt.Errorf("build reservation store: %w", err)
		}
		d, a, err := factory.New(c, restCfg, m)
		if err != nil {
			return nil, err
		}
		cs := NewClusterStore(c, d, a, repo)
		cs.SetReservationStore(reservations)
		return cs, nil
	}
}

// ReservationStore returns the reservation persistence of env; false when the env is unknown
// or has none wired.
func (m *MultiStore) ReservationStore(env string) (k8s.KeyStore, bool) {
	cs, ok := m.storeFor(env)
	if !ok || cs.reservations == nil {
		return nil, false
	}
	return cs.reservations, true
}

// NewMultiStoreFromClusterStores aggregates prebuilt ClusterStores (mainly for tests).
func NewMultiStoreFromClusterStores(reg *config.Config, m metrics.MetricsManager, stores map[string]*ClusterStore, stopAllTimeout time.Duration) (*MultiStore, error) {
	if len(stores) == 0 {
		return nil, fmt.Errorf("multi-store: no cluster stores provided")
	}
	return &MultiStore{registry: reg, stores: stores, metrics: m, stopAllTimeout: stopAllTimeout}, nil
}

// bootstrapTimeout bounds the initial sync of one cluster, at startup and on hot reload.
const bootstrapTimeout = 30 * time.Second

// Bootstrap calls ClusterStore.Bootstrap of all clusters in parallel.
// One cluster failing only logs a warning and sets cluster_reachable=0; error only if all fail.
func (m *MultiStore) Bootstrap(ctx context.Context) error {
	var (
		mu        sync.Mutex
		successes int
	)
	_, stores := m.snapshot()
	g, gctx := errgroup.WithContext(ctx)
	for env, cs := range stores {
		g.Go(func() error {
			cctx, cancel := context.WithTimeout(gctx, bootstrapTimeout)
			defer cancel()
			if err := cs.Bootstrap(cctx); err != nil {
				slog.Warn("cluster bootstrap failed", slog.String("cluster.env", env), slog.String("cluster.name", cs.cluster.Name), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "bootstrap"))
				m.metrics.SetReachable(cs.cluster.Name, 0)
				return nil
			}
			m.metrics.SetReachable(cs.cluster.Name, 1)
			mu.Lock()
			successes++
			mu.Unlock()
			return nil
		})
	}
	// Errors are absorbed inside the goroutines, so Wait always returns nil;
	// kept explicit so a future return err is not swallowed.
	_ = g.Wait()
	if successes == 0 {
		return fmt.Errorf("multi-store: bootstrap failed for all %d clusters", len(stores))
	}
	return nil
}

// Clusters returns all registry clusters as ClusterRefs.
func (m *MultiStore) Clusters() []model.ClusterRef {
	reg, _ := m.snapshot()
	clusters := reg.All()
	out := make([]model.ClusterRef, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, model.ClusterRef{Env: c.Env, Name: c.Name})
	}
	return out
}

// Namespaces returns the namespaces of the given cluster, or nil if absent.
func (m *MultiStore) Namespaces(env string) []string {
	cs, ok := m.storeFor(env)
	if !ok {
		return nil
	}
	return cs.Namespaces()
}

// WorkloadsByNamespace returns the workloads of the given cluster / namespace.
func (m *MultiStore) WorkloadsByNamespace(env, ns string) []*model.Workload {
	cs, ok := m.storeFor(env)
	if !ok {
		return nil
	}
	wls := cs.WorkloadsByNamespace(ns)
	for _, w := range wls {
		w.Env = env
	}
	return wls
}

// Start sets the workload of the given cluster / ns / name to running.
func (m *MultiStore) Start(env, ns, name, user string, stopAt time.Time) error {
	cs, ok := m.storeFor(env)
	if !ok {
		return model.ErrWorkloadNotFound
	}
	return cs.Start(ns, name, user, stopAt)
}

// SetStopAt moves the auto-stop of a running workload of the given cluster.
func (m *MultiStore) SetStopAt(env, ns, name string, stopAt time.Time) bool {
	cs, ok := m.storeFor(env)
	if !ok {
		return false
	}
	return cs.SetStopAt(ns, name, stopAt)
}

// Stop sets the workload of the given cluster / ns / name to stopped.
func (m *MultiStore) Stop(env, ns, name string) error {
	cs, ok := m.storeFor(env)
	if !ok {
		return model.ErrWorkloadNotFound
	}
	return cs.Stop(ns, name)
}

// StopAll runs StopAll on all registered clusters in parallel, stopping only what allow accepts.
// One cluster failing does not affect the others. The per-cluster timeout is m.stopAllTimeout.
func (m *MultiStore) StopAll(ctx context.Context, allow model.StopFilter) model.StopAllResult {
	_, stores := m.snapshot()
	var mu sync.Mutex
	result := model.StopAllResult{
		PerCluster: make(map[string]model.ClusterStopResult, len(stores)),
	}
	g, gctx := errgroup.WithContext(ctx)
	for key, cs := range stores {
		g.Go(func() error {
			cctx, cancel := context.WithTimeout(gctx, m.stopAllTimeout)
			defer cancel()
			var perCluster func(ns, name string) bool
			if allow != nil {
				perCluster = func(ns, name string) bool { return allow(key, ns, name) }
			}
			stopped, skipped, err := cs.StopAll(cctx, perCluster)
			mu.Lock()
			defer mu.Unlock()
			r := model.ClusterStopResult{Stopped: stopped, Skipped: skipped}
			if err != nil {
				r.Error = err.Error()
			} else {
				result.TotalStopped += stopped
			}
			result.PerCluster[key] = r
			return nil
		})
	}
	// Goroutines return no error, so Wait returns nil (per-cluster errors go to result.PerCluster).
	_ = g.Wait()
	return result
}

// Reconcile calls reconcile on the given cluster. Error if the cluster is absent.
func (m *MultiStore) Reconcile(ctx context.Context, env string) error {
	cs, ok := m.storeFor(env)
	if !ok {
		return fmt.Errorf("cluster %q not found", env)
	}
	return cs.Reconcile(ctx)
}

// ReconcileAll reconciles all clusters in parallel; one failure does not stop the rest.
func (m *MultiStore) ReconcileAll(ctx context.Context) error {
	_, stores := m.snapshot()
	g, gctx := errgroup.WithContext(ctx)
	for key, cs := range stores {
		g.Go(func() error {
			if err := cs.Reconcile(gctx); err != nil {
				slog.Warn("cluster reconcile failed", slog.String("cluster.env", key), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "reconcile"))
			}
			return nil
		})
	}
	// Goroutines absorb errors, so Wait returns nil.
	_ = g.Wait()
	return nil
}
