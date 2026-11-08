package model

import "context"

// Workload is the state of a workload
type Workload struct {
	Env       string // owning Cluster.Env
	Name      string
	Namespace string
	Kind      string // Deployment or Rollout
	Replicas  int
	StartedBy string
}

// ClusterRef is a lightweight DTO for the Bot / API layer to reference a Cluster
type ClusterRef struct {
	Env  string // env identifier (e.g. stg, stg-us, dev)
	Name string // EKS cluster name
}

// StopAllResult is the overall result of StopAll
type StopAllResult struct {
	TotalStopped int                          // total workloads stopped across all clusters
	PerCluster   map[string]ClusterStopResult // result per cluster key
}

// ClusterStopResult is the StopAll result of a single cluster
type ClusterStopResult struct {
	Stopped int    // workloads stopped in this cluster
	Error   string // error message ("" on success)
}

// WorkloadStore is the interface for workload state operations across clusters.
// Implemented by MultiStore in production and by an in-memory store in mocks.
type WorkloadStore interface {
	// Clusters returns references to all registered clusters.
	Clusters() []ClusterRef
	// Namespaces returns the managed namespaces of the given env.
	Namespaces(env string) []string
	// WorkloadsByNamespace returns the workloads of the given env / namespace.
	WorkloadsByNamespace(env, ns string) []*Workload
	// Start sets the workload to running (replicas=1, startedBy=user).
	Start(env, ns, name, user string) bool
	// Stop sets the workload to stopped (replicas=0).
	Stop(env, ns, name string) bool
	// StopAll stops workloads of all clusters in parallel.
	// A failure in one cluster does not stop the others; it is recorded in the result.
	StopAll(ctx context.Context) StopAllResult
	// Reconcile refreshes the state of the given env (called on Slack commands).
	Reconcile(ctx context.Context, env string) error
	// ReconcileAll reconciles all clusters in parallel (called at startup Bootstrap).
	ReconcileAll(ctx context.Context) error
}
