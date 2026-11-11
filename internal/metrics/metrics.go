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
	// SetGitOpsConflict sets 1 while a GitOps manager owns spec.replicas of the workload, 0 once resolved.
	SetGitOpsConflict(env, cluster, namespace, workload string, value float64)
	DeleteGitOpsConflict(env, cluster, namespace, workload string)
	SetReachable(cluster string, value float64)
	IncTokenRefresh(cluster, result string)
	// ObserveOperation records one workload operation (operation: start|stop|stop_all, result: success|error|not_found).
	ObserveOperation(env, cluster, operation, result string, d time.Duration)
	// DeleteCluster drops the activation series of env/cluster and the reachable series of cluster.
	DeleteCluster(env, cluster string)
	// IncConfigReload counts one hot reload attempt (target: registry|authz, result: success|error).
	IncConfigReload(target, result string)
	// IncReconcile counts one background reconcile of a cluster (result: success|error).
	IncReconcile(cluster, result string)
	// IncAutoStop counts one automatic stop of an expired workload (result: success|error).
	IncAutoStop(env, cluster, result string)
	// SetExternalScalerStreams sets the number of open KEDA external scaler streams of a cluster.
	SetExternalScalerStreams(cluster string, n int)
	// SetReservations sets the number of reservations of env in a state.
	SetReservations(env, state string, n int)
	// IncReservationExecution counts one reservation run (result: success|partial|failed|missed).
	IncReservationExecution(env, result string)
	Handler() http.Handler
}

// Manager implements MetricsManager
type Manager struct {
	activation   *prometheus.GaugeVec
	gitopsConfl  *prometheus.GaugeVec
	reachable    *prometheus.GaugeVec
	tokenRefresh *prometheus.CounterVec
	operations   *prometheus.CounterVec
	configReload *prometheus.CounterVec
	reconcile    *prometheus.CounterVec
	autoStops    *prometheus.CounterVec
	extStreams   *prometheus.GaugeVec
	// reservations / reservationRuns are the reservation gauge and execution counter.
	reservations    *prometheus.GaugeVec
	reservationRuns *prometheus.CounterVec
	opDuration      *prometheus.HistogramVec
	registry        *prometheus.Registry
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

	gitopsConflicts := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workload_knocker_gitops_conflicts",
			Help: "GitOps conflict of the direct actuator: 1 = a GitOps manager owns spec.replicas so the workload is not scaled, 0 = resolved",
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

	configReload := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workload_knocker_config_reload_total",
			Help: "Total number of configuration hot reload attempts, labeled by target (registry, authz) and result (success, error)",
		},
		[]string{"target", "result"},
	)
	// Pre-create every series so a rate() works before the first reload.
	for _, target := range []string{"registry", "authz"} {
		for _, result := range []string{"success", "error"} {
			configReload.WithLabelValues(target, result)
		}
	}

	reconcile := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workload_knocker_reconcile_total",
			Help: "Total number of background reconciles, labeled by cluster and result (success, error)",
		},
		[]string{"cluster", "result"},
	)

	autoStops := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workload_knocker_auto_stops_total",
			Help: "Total number of automatic stops of workloads whose run duration ended, labeled by env, cluster and result (success, error)",
		},
		[]string{"env", "cluster", "result"},
	)

	extStreams := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workload_knocker_external_scaler_streams",
			Help: "Number of open KEDA external scaler push streams, labeled by cluster",
		},
		[]string{"cluster"},
	)

	registry.MustRegister(activation)
	registry.MustRegister(gitopsConflicts)
	registry.MustRegister(reachable)
	registry.MustRegister(tokenRefresh)
	registry.MustRegister(operations)
	registry.MustRegister(opDuration)
	registry.MustRegister(configReload)
	registry.MustRegister(reconcile)
	reservations := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workload_knocker_reservations",
			Help: "Number of one-off reservations, labeled by env and state (pending, started, failed, missed, cancelled; finished ones are kept for a day)",
		},
		[]string{"env", "state"},
	)

	reservationRuns := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workload_knocker_reservation_executions_total",
			Help: "Total number of reservation runs, labeled by env and result (success, partial, failed, missed)",
		},
		[]string{"env", "result"},
	)

	registry.MustRegister(autoStops)
	registry.MustRegister(extStreams)
	registry.MustRegister(reservations)
	registry.MustRegister(reservationRuns)

	return &Manager{
		reservations:    reservations,
		reservationRuns: reservationRuns,
		activation:      activation,
		gitopsConfl:     gitopsConflicts,
		reachable:       reachable,
		tokenRefresh:    tokenRefresh,
		operations:      operations,
		configReload:    configReload,
		reconcile:       reconcile,
		autoStops:       autoStops,
		extStreams:      extStreams,
		opDuration:      opDuration,
		registry:        registry,
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

// SetGitOpsConflict sets the GitOps conflict Gauge of a workload.
func (m *Manager) SetGitOpsConflict(env, cluster, namespace, workload string, value float64) {
	m.gitopsConfl.WithLabelValues(env, cluster, namespace, workload).Set(value)
}

// DeleteGitOpsConflict removes the GitOps conflict series of a workload.
func (m *Manager) DeleteGitOpsConflict(env, cluster, namespace, workload string) {
	m.gitopsConfl.DeleteLabelValues(env, cluster, namespace, workload)
}

// DeleteCluster removes the series of a cluster that left the registry.
func (m *Manager) DeleteCluster(env, cluster string) {
	m.activation.DeletePartialMatch(prometheus.Labels{"env": env, "cluster": cluster})
	m.gitopsConfl.DeletePartialMatch(prometheus.Labels{"env": env, "cluster": cluster})
	m.reachable.DeleteLabelValues(cluster)
	m.extStreams.DeleteLabelValues(cluster)
	m.reservations.DeletePartialMatch(prometheus.Labels{"env": env})
}

// SetExternalScalerStreams sets the open stream count of a cluster.
func (m *Manager) SetExternalScalerStreams(cluster string, n int) {
	m.extStreams.WithLabelValues(cluster).Set(float64(n))
}

// IncConfigReload counts one hot reload attempt.
func (m *Manager) IncConfigReload(target, result string) {
	m.configReload.WithLabelValues(target, result).Inc()
}

// IncReconcile counts one background reconcile of a cluster.
func (m *Manager) IncReconcile(cluster, result string) {
	m.reconcile.WithLabelValues(cluster, result).Inc()
}

// IncAutoStop counts one automatic stop.
func (m *Manager) IncAutoStop(env, cluster, result string) {
	m.autoStops.WithLabelValues(env, cluster, result).Inc()
}

// SetReservations sets the reservation count of env in a state.
func (m *Manager) SetReservations(env, state string, n int) {
	m.reservations.WithLabelValues(env, state).Set(float64(n))
}

// IncReservationExecution counts one reservation run.
func (m *Manager) IncReservationExecution(env, result string) {
	m.reservationRuns.WithLabelValues(env, result).Inc()
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
