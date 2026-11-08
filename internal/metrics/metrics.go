package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsManager is the interface managing Prometheus metrics
type MetricsManager interface {
	SetActivation(env, cluster, namespace, workload string, value float64)
	DeleteMetric(env, cluster, namespace, workload string)
	SetReachable(cluster string, value float64)
	IncTokenRefresh(cluster, result string)
	// ObserveOperation records one workload operation (operation: start|stop|stop_all, result: success|error|not_found).
	ObserveOperation(env, cluster, operation, result string, d time.Duration)
	Handler() http.Handler
}

// Manager implements MetricsManager
type Manager struct {
	activation   *prometheus.GaugeVec
	reachable    *prometheus.GaugeVec
	tokenRefresh *prometheus.CounterVec
	operations   *prometheus.CounterVec
	opDuration   *prometheus.HistogramVec
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

	operations := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workload_knocker_operations_total",
			Help: "Total number of workload operations, labeled by env, cluster, operation (start, stop, stop_all) and result (success, error, not_found)",
		},
		[]string{"env", "cluster", "operation", "result"},
	)

	opDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workload_knocker_operation_duration_seconds",
			Help:    "Duration of workload operations in seconds, labeled by env, cluster and operation",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"env", "cluster", "operation"},
	)

	registry.MustRegister(activation)
	registry.MustRegister(reachable)
	registry.MustRegister(tokenRefresh)
	registry.MustRegister(operations)
	registry.MustRegister(opDuration)

	return &Manager{
		activation:   activation,
		reachable:    reachable,
		tokenRefresh: tokenRefresh,
		operations:   operations,
		opDuration:   opDuration,
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

// ObserveOperation counts one workload operation and records its duration.
// Labels are bounded (env, cluster, fixed operation/result sets); never pass user IDs or workload names.
func (m *Manager) ObserveOperation(env, cluster, operation, result string, d time.Duration) {
	m.operations.WithLabelValues(env, cluster, operation, result).Inc()
	m.opDuration.WithLabelValues(env, cluster, operation).Observe(d.Seconds())
}

// Handler returns the http.Handler exposing Prometheus metrics
func (m *Manager) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
