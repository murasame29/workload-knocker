// Package actuator defines how a cluster's managed workloads are discovered and how their desired
// state is applied. Implementations live in subpackages and are selected per cluster through
// config.Cluster.Actuator (see internal/actuator/factory).
package actuator

import (
	"context"
	"errors"
)

// Workload identifies one managed workload.
type Workload struct {
	Namespace string
	Name      string
	// Kind is the scale target kind (Deployment or StatefulSet).
	Kind string
	// Annotations carry the per-workload settings (run durations, ready message) of the managing object.
	Annotations map[string]string
}

// ErrReplicasUnknown is returned by Discoverer.Replicas when the implementation cannot report the
// current replicas; the workload then starts as stopped without a warning.
var ErrReplicasUnknown = errors.New("current replicas unknown")

// Refusal is implemented by Apply errors caused by the workload's own configuration (for example a
// GitOps manager owning spec.replicas). The message is shown to the user and the state is not kept.
type Refusal interface {
	error
	Refused()
}

// RefusedError is a Refusal with a plain reason.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return e.Reason }

// Refused marks e as a Refusal.
func (*RefusedError) Refused() {}

// AsRefusal returns the Refusal in err's chain, if any.
func AsRefusal(err error) (Refusal, bool) {
	var r Refusal
	ok := errors.As(err, &r)
	return r, ok
}

// Discoverer finds the workloads of one cluster that workload-knocker manages.
type Discoverer interface {
	// Discover lists every managed workload. A workload absent from the result is unmanaged.
	Discover(ctx context.Context) ([]Workload, error)
	// Replicas returns the current replicas of w, used as the initial state of a newly discovered workload.
	// A read failure makes the workload start as stopped.
	Replicas(ctx context.Context, w Workload) (int32, error)
}

// Actuator applies the desired state of the workloads of one cluster.
// Implementations must be safe for concurrent use and idempotent.
type Actuator interface {
	// Apply makes w start (running=true) or stop (running=false).
	Apply(ctx context.Context, w Workload, running bool) error
	// Forget releases what Apply published for w once it is no longer managed or its cluster left.
	Forget(ctx context.Context, w Workload) error
}
