package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/rest"

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
	registry       *config.Config
	stores         map[string]*ClusterStore // key = Cluster.Env
	metrics        metrics.MetricsManager
	stopAllTimeout time.Duration
}

// Compile-time interface check.
var _ model.WorkloadStore = (*MultiStore)(nil)

// NewMultiStore eagerly builds a ClusterStore for each registered cluster.
// stopAllTimeout is the per-cluster timeout of StopAll.
// It passes the *rest.Config from RestConfigSource to k8s.NewClusterAccess and injects the resulting
// ScaledObjectLister + StateRepository into the ClusterStore.
// A client creation failure only logs a warning and skips that cluster; error only if all fail.
func NewMultiStore(ctx context.Context, reg *config.Config, src RestConfigSource, m metrics.MetricsManager, stopAllTimeout time.Duration) (*MultiStore, error) {
	controllerNs := reg.Controller.Namespace
	stores := make(map[string]*ClusterStore)
	for _, c := range reg.All() {
		restCfg, err := src.RestConfig(ctx, c)
		if err != nil {
			slog.Warn("failed to build rest.Config", slog.String("cluster.env", c.Env), slog.String("cluster.name", c.Name), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "build-rest-config"))
			m.SetReachable(c.Name, 0)
			continue
		}
		lister, repo, err := k8s.NewClusterAccess(restCfg, controllerNs)
		if err != nil {
			slog.Warn("failed to build cluster access", slog.String("cluster.env", c.Env), slog.String("cluster.name", c.Name), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "build-cluster-access"))
			m.SetReachable(c.Name, 0)
			continue
		}
		stores[c.Env] = NewClusterStore(c, lister, repo, m)
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

// NewMultiStoreFromClusterStores aggregates prebuilt ClusterStores (mainly for tests).
func NewMultiStoreFromClusterStores(reg *config.Config, m metrics.MetricsManager, stores map[string]*ClusterStore, stopAllTimeout time.Duration) (*MultiStore, error) {
	if len(stores) == 0 {
		return nil, fmt.Errorf("multi-store: no cluster stores provided")
	}
	return &MultiStore{registry: reg, stores: stores, metrics: m, stopAllTimeout: stopAllTimeout}, nil
}

// Bootstrap calls ClusterStore.Bootstrap of all clusters in parallel.
// One cluster failing only logs a warning and sets cluster_reachable=0; error only if all fail.
func (m *MultiStore) Bootstrap(ctx context.Context) error {
	var (
		mu        sync.Mutex
		successes int
	)
	g, gctx := errgroup.WithContext(ctx)
	for env, cs := range m.stores {
		g.Go(func() error {
			cctx, cancel := context.WithTimeout(gctx, 30*time.Second)
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
		return fmt.Errorf("multi-store: bootstrap failed for all %d clusters", len(m.stores))
	}
	return nil
}

// Clusters returns all registry clusters as ClusterRefs.
func (m *MultiStore) Clusters() []model.ClusterRef {
	clusters := m.registry.All()
	out := make([]model.ClusterRef, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, model.ClusterRef{Env: c.Env, Name: c.Name})
	}
	return out
}

// Namespaces returns the namespaces of the given cluster, or nil if absent.
func (m *MultiStore) Namespaces(env string) []string {
	cs, ok := m.stores[env]
	if !ok {
		return nil
	}
	return cs.Namespaces()
}

// WorkloadsByNamespace returns the workloads of the given cluster / namespace.
func (m *MultiStore) WorkloadsByNamespace(env, ns string) []*model.Workload {
	cs, ok := m.stores[env]
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
func (m *MultiStore) Start(env, ns, name, user string) bool {
	cs, ok := m.stores[env]
	if !ok {
		return false
	}
	return cs.Start(ns, name, user)
}

// Stop sets the workload of the given cluster / ns / name to stopped.
func (m *MultiStore) Stop(env, ns, name string) bool {
	cs, ok := m.stores[env]
	if !ok {
		return false
	}
	return cs.Stop(ns, name)
}

// StopAll runs StopAll on all registered clusters in parallel.
// One cluster failing does not affect the others. The per-cluster timeout is m.stopAllTimeout.
func (m *MultiStore) StopAll(ctx context.Context) model.StopAllResult {
	var mu sync.Mutex
	result := model.StopAllResult{
		PerCluster: make(map[string]model.ClusterStopResult, len(m.stores)),
	}
	g, gctx := errgroup.WithContext(ctx)
	for key, cs := range m.stores {
		g.Go(func() error {
			cctx, cancel := context.WithTimeout(gctx, m.stopAllTimeout)
			defer cancel()
			stopped, err := cs.StopAll(cctx)
			mu.Lock()
			defer mu.Unlock()
			r := model.ClusterStopResult{Stopped: stopped}
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
	cs, ok := m.stores[env]
	if !ok {
		return fmt.Errorf("cluster %q not found", env)
	}
	return cs.Reconcile(ctx)
}

// ReconcileAll reconciles all clusters in parallel; one failure does not stop the rest.
func (m *MultiStore) ReconcileAll(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	for key, cs := range m.stores {
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
