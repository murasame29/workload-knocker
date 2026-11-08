package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsManager is the interface managing Prometheus metrics
type MetricsManager interface {
	SetActivation(env, cluster, namespace, workload string, value float64)
	DeleteMetric(env, cluster, namespace, workload string)
	SetReachable(cluster string, value float64)
	IncTokenRefresh(cluster, result string)
	Handler() http.Handler
}

// Manager implements MetricsManager
type Manager struct {
	activation   *prometheus.GaugeVec
	reachable    *prometheus.GaugeVec
	tokenRefresh *prometheus.CounterVec
	registry     *prometheus.Registry
}

// New creates a Manager and registers each metric with the Prometheus registry
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

// SetActivation sets the value of a labeled Gauge
func (m *Manager) SetActivation(env, cluster, namespace, workload string, value float64) {
	m.activation.WithLabelValues(env, cluster, namespace, workload).Set(value)
}

// DeleteMetric removes the series with the given labels from a Gauge
func (m *Manager) DeleteMetric(env, cluster, namespace, workload string) {
	m.activation.DeleteLabelValues(env, cluster, namespace, workload)
}

// SetReachable updates the reachability Gauge of the given cluster
func (m *Manager) SetReachable(cluster string, value float64) {
	m.reachable.WithLabelValues(cluster).Set(value)
}

// IncTokenRefresh increments the token refresh Counter by 1. result is "success" or "error"
func (m *Manager) IncTokenRefresh(cluster, result string) {
	m.tokenRefresh.WithLabelValues(cluster, result).Inc()
}

// Handler returns the http.Handler exposing Prometheus metrics
func (m *Manager) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
