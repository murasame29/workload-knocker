// Package kedaprometheus is the default actuator: it discovers ScaledObjects whose Prometheus
// trigger queries workload_knocker_activation and publishes that gauge for KEDA to scale on.
package kedaprometheus

import (
	"context"
	"fmt"
	"strings"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/metrics"
)

// activationTriggerMetric is the metric name that managed Prometheus trigger queries contain.
const activationTriggerMetric = "workload_knocker_activation"

// Backend implements actuator.Discoverer and actuator.Actuator for one cluster.
type Backend struct {
	cluster config.Cluster
	lister  k8s.ScaledObjectLister
	targets k8s.ScaleTargetReader // nil: Replicas reports actuator.ErrReplicasUnknown
	metrics metrics.MetricsManager
}

var (
	_ actuator.Discoverer = (*Backend)(nil)
	_ actuator.Actuator   = (*Backend)(nil)
)

// New returns a Backend. targets may be nil, then newly discovered workloads start as stopped.
func New(c config.Cluster, lister k8s.ScaledObjectLister, targets k8s.ScaleTargetReader, m metrics.MetricsManager) *Backend {
	return &Backend{cluster: c, lister: lister, targets: targets, metrics: m}
}

// Discover lists the ScaledObjects that carry an activation trigger.
func (b *Backend) Discover(ctx context.Context) ([]actuator.Workload, error) {
	items, err := b.lister.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list ScaledObjects: %w", err)
	}
	var out []actuator.Workload
	for i := range items {
		so := &items[i]
		if !hasActivationTrigger(so) {
			continue
		}
		out = append(out, actuator.Workload{
			Namespace:   so.Namespace,
			Name:        so.Spec.ScaleTargetRef.Name,
			Kind:        scaleTargetKind(so),
			Annotations: so.Annotations,
		})
	}
	return out, nil
}

// Replicas reads spec.replicas of the scale target.
func (b *Backend) Replicas(ctx context.Context, w actuator.Workload) (int32, error) {
	if b.targets == nil {
		return 0, actuator.ErrReplicasUnknown
	}
	return b.targets.Replicas(ctx, w.Kind, w.Namespace, w.Name)
}

// Apply sets the activation gauge to 1 (running) or 0 (stopped).
func (b *Backend) Apply(_ context.Context, w actuator.Workload, running bool) error {
	var v float64
	if running {
		v = 1
	}
	b.metrics.SetActivation(b.cluster.Env, b.cluster.Name, w.Namespace, w.Name, v)
	return nil
}

// Forget deletes the activation series of w.
func (b *Backend) Forget(_ context.Context, w actuator.Workload) error {
	b.metrics.DeleteMetric(b.cluster.Env, b.cluster.Name, w.Namespace, w.Name)
	return nil
}

func scaleTargetKind(so *kedav1alpha1.ScaledObject) string {
	if k := so.Spec.ScaleTargetRef.Kind; k != "" {
		return k
	}
	return "Deployment"
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
