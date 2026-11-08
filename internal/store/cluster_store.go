// Package store はワークロード起動状態のドメインロジックを担う。
//
// 設計意図:
//   - K8s API への直接依存は internal/k8s に閉じ込め、ドメイン層は ScaledObjectLister /
//     StateRepository の interface 越しに使う
//   - 単一 cluster の責務は ClusterStore、複数 cluster のオーケストレーションは MultiStore
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
	nrx "github.com/murasame29/workload-knocker/pkg/newrelic"
)

// activationTriggerMetric は workload-knocker が管理対象とする Prometheus trigger query に
// 含まれているべきメトリクス名。
const activationTriggerMetric = "workload_knocker_activation"

// ClusterStore は単一 managed cluster のワークロード状態を管理する。
// MultiStore に集約されることで model.WorkloadStore を構成する。
//
// ロック設計:
//   - mu は workloads マップへの read/write を保護する (短時間)。K8s API call は保持しない。
//   - persistMu は state.Save の直列化を保証する。Start/Stop 等から呼ぶ際は mu 解放後に取得する。
type ClusterStore struct {
	mu        sync.RWMutex
	persistMu sync.Mutex
	cluster   config.Cluster
	scaledObj k8s.ScaledObjectLister
	state     k8s.StateRepository
	metrics   metrics.MetricsManager
	tracer    nrx.Tracer
	workloads map[string]*model.Workload // key: "namespace/name"
}

// NewClusterStore は新しい ClusterStore を生成する。
func NewClusterStore(c config.Cluster, scaledObj k8s.ScaledObjectLister, state k8s.StateRepository, m metrics.MetricsManager, tracer nrx.Tracer) *ClusterStore {
	return &ClusterStore{
		cluster:   c,
		scaledObj: scaledObj,
		state:     state,
		metrics:   m,
		tracer:    tracer,
		workloads: make(map[string]*model.Workload),
	}
}

// Namespaces はレジストリ内の全 namespace を返す。
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

// WorkloadsByNamespace は指定 namespace のワークロード一覧を返す。
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

// Start はワークロードを起動状態にする。インメモリ更新 → 永続化 → メトリクスを順に行う。
// 永続化 (K8s API call) は mu を解放してから persistMu の下で実行する。
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

// Stop はワークロードを停止状態にする。
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

// StopAll は全ワークロードを停止する。戻り値は呼び出し時点で起動中だった workload 数。
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
	// メトリクス送信用に namespace/name のリストを取得しておく
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

// Bootstrap は起動時に一度だけ呼び出す初期化処理。
// ScaledObject を list してレジストリを構築した後、ConfigMap から状態を復元する。
func (s *ClusterStore) Bootstrap(ctx context.Context) error {
	if err := s.reconcile(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreLocked(ctx)
	return nil
}

// Reconcile は ScaledObject を list してレジストリを最新化する。
func (s *ClusterStore) Reconcile(ctx context.Context) error {
	return s.reconcile(ctx)
}

func (s *ClusterStore) reconcile(ctx context.Context) error {
	span := s.tracer.Start("K8s/ScaledObject/List")
	items, err := s.scaledObj.List(ctx)
	span.End()
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

// handleScaledObjectLocked は ScaledObject をレジストリに登録/更新/削除する。mu 保持必須。
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

// restoreLocked は state repository から状態を復元する。mu 保持必須。
func (s *ClusterStore) restoreLocked(ctx context.Context) {
	span := s.tracer.Start("K8s/State/Load")
	entries, err := s.state.Load(ctx)
	span.End()
	if err != nil {
		slog.Warn("failed to load workload state", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "load-state"))
		return
	}

	for key, entry := range entries {
		w, ok := s.workloads[key]
		if !ok {
			// レジストリに存在しないエントリは無視する
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

// snapshotEntriesLocked は現在のインメモリ状態のスナップショットを返す。mu (read or write) 保持必須。
// 永続化は呼び出し側が mu 解放後に persist へ渡す。
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

// persist はスナップショットを state repository に保存する。mu を保持してはならない。
// persistMu で直列化することで Save の競合更新を避ける。
func (s *ClusterStore) persist(entries map[string]k8s.StateEntry) {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	span := s.tracer.Start("K8s/State/Save")
	err := s.state.Save(context.Background(), entries)
	span.End()
	if err != nil {
		slog.Warn("failed to save workload state", slog.String("cluster.name", s.cluster.Name), slog.String("cluster.env", s.cluster.Env), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "save-state"))
	}
}
