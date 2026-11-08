package model

import "context"

// Workload はワークロードの状態を表す
type Workload struct {
	Env       string // 所属する Cluster.Env
	Name      string
	Namespace string
	Kind      string // Deployment or Rollout
	Replicas  int
	StartedBy string
}

// ClusterRef は Bot / API レイヤが Cluster を参照するための軽量 DTO
type ClusterRef struct {
	Env  string // env 識別子 (例: stg, stg-us, dev)
	Name string // EKS cluster name
}

// StopAllResult は StopAll の全体結果
type StopAllResult struct {
	TotalStopped int                          // 全 cluster で停止した workload 数の合計
	PerCluster   map[string]ClusterStopResult // cluster key 単位の結果
}

// ClusterStopResult は単一 cluster の StopAll 結果
type ClusterStopResult struct {
	Stopped int    // この cluster で停止した workload 数
	Error   string // エラーメッセージ ("" のとき成功)
}

// WorkloadStore は複数 cluster を横断するワークロード状態の操作インターフェース
// 本番では MultiStore (各 ClusterStore を集約) が実装、モックではインメモリ実装。
type WorkloadStore interface {
	// Clusters は登録された全 cluster の参照を返す。
	Clusters() []ClusterRef
	// Namespaces は指定 env で管理対象の namespace 一覧を返す。
	Namespaces(env string) []string
	// WorkloadsByNamespace は指定 env / namespace の workload 一覧を返す。
	WorkloadsByNamespace(env, ns string) []*Workload
	// Start は指定 workload を起動状態にする (replicas=1, startedBy=user)。
	Start(env, ns, name, user string) bool
	// Stop は指定 workload を停止状態にする (replicas=0)。
	Stop(env, ns, name string) bool
	// StopAll は全 cluster の workload を並列に停止する。
	// 1 cluster の失敗は他 cluster の処理を止めず、結果に格納する。
	StopAll(ctx context.Context) StopAllResult
	// Reconcile は指定 env の状態を最新化する (Slack コマンド受信時に呼ぶ)。
	Reconcile(ctx context.Context, env string) error
	// ReconcileAll は全 cluster を並列に Reconcile する (起動時 Bootstrap で呼ぶ)。
	ReconcileAll(ctx context.Context) error
}
