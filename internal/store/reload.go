package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/murasame29/workload-knocker/internal/config"
)

// ReloadResult lists the envs touched by Reload.
type ReloadResult struct {
	Added   []string
	Removed []string
	Changed []string
	// Kept are changed clusters whose new connection failed to bootstrap; their old store stays.
	Kept []string
}

// Empty reports whether the reload changed no cluster.
func (r ReloadResult) Empty() bool {
	return len(r.Added)+len(r.Removed)+len(r.Changed)+len(r.Kept) == 0
}

type builtStore struct {
	env string
	cs  *ClusterStore
	// err: the store could not be built. bootErr: built but the initial sync failed.
	err, bootErr error
}

// Reload swaps in next: it builds and bootstraps stores for added or changed clusters, then
// replaces the registry and store set atomically. In-flight calls finish on the old set.
// Like at startup, an added cluster whose initial sync fails is kept with reachable=0 and recovers
// on the next Reconcile; a changed cluster that fails keeps its old store.
func (m *MultiStore) Reload(ctx context.Context, next *config.Config, build ClusterFactory) (ReloadResult, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	oldReg, oldStores := m.snapshot()
	var res ReloadResult
	var todo []config.Cluster
	isChanged := map[string]bool{}
	for _, c := range next.All() {
		old, ok := oldReg.ByEnv(c.Env)
		switch {
		case !ok:
			res.Added = append(res.Added, c.Env)
			todo = append(todo, c)
		case !old.SameBackend(c):
			res.Changed = append(res.Changed, c.Env)
			isChanged[c.Env] = true
			todo = append(todo, c)
		}
	}
	for _, c := range oldReg.All() {
		if _, ok := next.ByEnv(c.Env); !ok {
			res.Removed = append(res.Removed, c.Env)
		}
	}

	built := m.buildAll(ctx, todo, next.Controller.Namespace, build)

	stores := make(map[string]*ClusterStore, len(next.Clusters))
	for _, c := range next.All() {
		if cs, ok := oldStores[c.Env]; ok {
			stores[c.Env] = cs
		}
	}
	var swapped []builtStore
	for _, b := range built {
		_, existed := oldStores[b.env]
		switch {
		case b.err == nil && b.bootErr == nil:
			stores[b.env] = b.cs
			swapped = append(swapped, b)
		case existed:
			res.Kept = append(res.Kept, b.env)
			res.Changed = slices.DeleteFunc(res.Changed, func(e string) bool { return e == b.env })
			slog.Warn("cluster reload failed, keeping the previous connection", slog.String("cluster.env", b.env), slog.String("error.message", errors.Join(b.err, b.bootErr).Error()), slog.String("operation", "reload"))
		case b.err == nil:
			stores[b.env] = b.cs
			m.setReachable(next, b.env, 0)
			slog.Warn("added cluster bootstrap failed", slog.String("cluster.env", b.env), slog.String("error.message", b.bootErr.Error()), slog.String("operation", "reload"))
		default:
			res.Added = slices.DeleteFunc(res.Added, func(e string) bool { return e == b.env })
			m.setReachable(next, b.env, 0)
			slog.Warn("added cluster could not be initialized", slog.String("cluster.env", b.env), slog.String("error.message", b.err.Error()), slog.String("operation", "reload"))
		}
	}
	if len(stores) == 0 {
		return ReloadResult{}, fmt.Errorf("multi-store: no cluster store is available after reload")
	}

	m.mu.Lock()
	m.registry, m.stores = next, stores
	m.mu.Unlock()

	for _, env := range res.Removed {
		if c, ok := oldReg.ByEnv(env); ok {
			m.metrics.DeleteCluster(env, c.Name)
		}
	}
	for _, b := range swapped {
		if old, ok := oldStores[b.env]; ok {
			m.dropStaleSeries(old, b.cs)
		}
	}
	slices.Sort(res.Added)
	slices.Sort(res.Removed)
	slices.Sort(res.Changed)
	slices.Sort(res.Kept)
	return res, nil
}

func (m *MultiStore) setReachable(reg *config.Config, env string, v float64) {
	if c, ok := reg.ByEnv(env); ok {
		m.metrics.SetReachable(c.Name, v)
	}
}

func (m *MultiStore) buildAll(ctx context.Context, clusters []config.Cluster, ns string, build ClusterFactory) []builtStore {
	out := make([]builtStore, len(clusters))
	var wg sync.WaitGroup
	for i, c := range clusters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, bootstrapTimeout)
			defer cancel()
			b := builtStore{env: c.Env}
			if b.cs, b.err = build(cctx, c, ns); b.err == nil {
				if b.bootErr = b.cs.Bootstrap(cctx); b.bootErr == nil {
					m.metrics.SetReachable(c.Name, 1)
				}
			}
			out[i] = b
		}()
	}
	wg.Wait()
	return out
}

// dropStaleSeries makes old forget the workloads next no longer owns, all of them when the actuator changed.
func (m *MultiStore) dropStaleSeries(old, next *ClusterStore) {
	if old.cluster.Name != next.cluster.Name {
		m.metrics.DeleteCluster(old.cluster.Env, old.cluster.Name)
		return
	}
	kept := map[string]struct{}{}
	if old.cluster.ActuatorType() == next.cluster.ActuatorType() {
		for _, w := range next.snapshot() {
			kept[w.Namespace+"/"+w.Name] = struct{}{}
		}
	}
	for _, w := range old.snapshot() {
		if _, ok := kept[w.Namespace+"/"+w.Name]; !ok {
			old.forget(ref(&w))
		}
	}
}
