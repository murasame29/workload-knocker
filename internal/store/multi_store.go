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
	nrx "github.com/murasame29/workload-knocker/pkg/newrelic"
)

// RestConfigSource は config.Cluster から *rest.Config を取得する抽象。
// 本番では k8sauth.Multiplexer が満たす。テストでは fake を差し込める。
type RestConfigSource interface {
	RestConfig(ctx context.Context, c config.Cluster) (*rest.Config, error)
}

// MultiStore は *config.Config に登録された全 ClusterStore を集約し model.WorkloadStore を実装する。
type MultiStore struct {
	registry       *config.Config
	stores         map[string]*ClusterStore // key = Cluster.Env
	metrics        metrics.MetricsManager
	stopAllTimeout time.Duration
}

// コンパイル時 interface 充足チェック
var _ model.WorkloadStore = (*MultiStore)(nil)

// NewMultiStore は registry に登録された各 cluster の ClusterStore を eager に構築する。
// stopAllTimeout は StopAll の per-cluster タイムアウト。
// RestConfigSource (k8sauth.Multiplexer) で取得した *rest.Config を k8s.NewClusterAccess に渡し、
// ScaledObjectLister + StateRepository を生成して ClusterStore に注入する。
// クライアント生成失敗は warning ログに留め、その cluster をスキップする。全 cluster 失敗時のみ error。
func NewMultiStore(ctx context.Context, reg *config.Config, src RestConfigSource, m metrics.MetricsManager, tracer nrx.Tracer, stopAllTimeout time.Duration) (*MultiStore, error) {
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
		stores[c.Env] = NewClusterStore(c, lister, repo, m, tracer)
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

// NewMultiStoreFromClusterStores は事前に構築済みの ClusterStore を集約する (主にテスト用)。
func NewMultiStoreFromClusterStores(reg *config.Config, m metrics.MetricsManager, stores map[string]*ClusterStore, stopAllTimeout time.Duration) (*MultiStore, error) {
	if len(stores) == 0 {
		return nil, fmt.Errorf("multi-store: no cluster stores provided")
	}
	return &MultiStore{registry: reg, stores: stores, metrics: m, stopAllTimeout: stopAllTimeout}, nil
}

// Bootstrap は全 cluster の ClusterStore.Bootstrap を並列に呼ぶ。
// 1 cluster の失敗は warn log + cluster_reachable=0 に留める。全 cluster 失敗時のみ error を返す。
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
	// goroutine 内で error を吸収しているため Wait の戻り値は常に nil。
	// 将来 return err を入れた際に握り潰さないよう明示。
	_ = g.Wait()
	if successes == 0 {
		return fmt.Errorf("multi-store: bootstrap failed for all %d clusters", len(m.stores))
	}
	return nil
}

// Clusters は registry の全 cluster を ClusterRef として返す。
func (m *MultiStore) Clusters() []model.ClusterRef {
	clusters := m.registry.All()
	out := make([]model.ClusterRef, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, model.ClusterRef{Env: c.Env, Name: c.Name})
	}
	return out
}

// Namespaces は指定 cluster の namespace 一覧を返す。cluster 不在時は nil。
func (m *MultiStore) Namespaces(env string) []string {
	cs, ok := m.stores[env]
	if !ok {
		return nil
	}
	return cs.Namespaces()
}

// WorkloadsByNamespace は指定 cluster / namespace の workload 一覧を返す。
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

// Start は指定 cluster / ns / name の workload を起動状態にする。
func (m *MultiStore) Start(env, ns, name, user string) bool {
	cs, ok := m.stores[env]
	if !ok {
		return false
	}
	return cs.Start(ns, name, user)
}

// Stop は指定 cluster / ns / name の workload を停止状態にする。
func (m *MultiStore) Stop(env, ns, name string) bool {
	cs, ok := m.stores[env]
	if !ok {
		return false
	}
	return cs.Stop(ns, name)
}

// StopAll は登録された全 cluster の StopAll を並列に実行する。
// 1 cluster の失敗は他 cluster の処理を継続する。per-cluster timeout は m.stopAllTimeout。
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
	// 各 goroutine は error を返さないので Wait は常に nil を返す (per-cluster の error は result.PerCluster に集約)。
	_ = g.Wait()
	return result
}

// Reconcile は指定 cluster の reconcile を呼ぶ。cluster 不在時は error。
func (m *MultiStore) Reconcile(ctx context.Context, env string) error {
	cs, ok := m.stores[env]
	if !ok {
		return fmt.Errorf("cluster %q not found", env)
	}
	return cs.Reconcile(ctx)
}

// ReconcileAll は全 cluster の reconcile を並列に呼ぶ。1 cluster 失敗で全体を止めない。
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
	// 各 goroutine は error を吸収するので Wait は常に nil を返す。
	_ = g.Wait()
	return nil
}
