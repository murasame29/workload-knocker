package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsManager は Prometheus メトリクスの管理インターフェース
type MetricsManager interface {
	SetActivation(env, cluster, namespace, workload string, value float64)
	DeleteMetric(env, cluster, namespace, workload string)
	SetReachable(cluster string, value float64)
	IncTokenRefresh(cluster, result string)
	Handler() http.Handler
}

// Manager は MetricsManager の実装
type Manager struct {
	activation   *prometheus.GaugeVec
	reachable    *prometheus.GaugeVec
	tokenRefresh *prometheus.CounterVec
	registry     *prometheus.Registry
}

// New は新しい Manager を生成し、Prometheus レジストリに各メトリクスを登録する
func New() *Manager {
	registry := prometheus.NewRegistry()

	activation := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workload_knocker_activation",
			Help: "Workload activation state: 1 = running, 0 = stopped",
		},
		[]string{"env", "cluster", "namespace", "workload"},
	)

	reachable := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workload_knocker_cluster_reachable",
			Help: "Cluster reachability: 1 = reachable, 0 = unreachable",
		},
		[]string{"cluster"},
	)

	tokenRefresh := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workload_knocker_token_refresh_total",
			Help: "Total number of bearer token refresh attempts, labeled by cluster and result (success or error)",
		},
		[]string{"cluster", "result"},
	)

	registry.MustRegister(activation)
	registry.MustRegister(reachable)
	registry.MustRegister(tokenRefresh)

	return &Manager{
		activation:   activation,
		reachable:    reachable,
		tokenRefresh: tokenRefresh,
		registry:     registry,
	}
}

// SetActivation はラベル付き Gauge の値を設定する
func (m *Manager) SetActivation(env, cluster, namespace, workload string, value float64) {
	m.activation.WithLabelValues(env, cluster, namespace, workload).Set(value)
}

// DeleteMetric は指定ラベルのメトリクス系列を Gauge から削除する
func (m *Manager) DeleteMetric(env, cluster, namespace, workload string) {
	m.activation.DeleteLabelValues(env, cluster, namespace, workload)
}

// SetReachable は指定 cluster の reachability Gauge を更新する
func (m *Manager) SetReachable(cluster string, value float64) {
	m.reachable.WithLabelValues(cluster).Set(value)
}

// IncTokenRefresh は token refresh Counter を 1 加算する。result は "success" または "error"
func (m *Manager) IncTokenRefresh(cluster, result string) {
	m.tokenRefresh.WithLabelValues(cluster, result).Inc()
}

// Handler は Prometheus メトリクスを公開する http.Handler を返す
func (m *Manager) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
