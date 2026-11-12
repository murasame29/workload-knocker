package kedaexternal

import (
	"context"
	"fmt"
	"strings"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/cluster/k8s"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
)

// externalTriggerTypes are the KEDA trigger types served by this actuator.
var externalTriggerTypes = map[string]bool{"external-push": true, "external": true}

// Backend implements actuator.Discoverer and actuator.Actuator for one cluster on a shared Hub.
type Backend struct {
	cluster config.Cluster
	lister  k8s.ScaledObjectLister
	targets k8s.ScaleTargetReader // nil: Replicas reports actuator.ErrReplicasUnknown
	hub     *Hub
}

var (
	_ actuator.Discoverer = (*Backend)(nil)
	_ actuator.Actuator   = (*Backend)(nil)
)

// New registers cluster c with hub and returns its Backend. targets may be nil, then newly
// discovered workloads start as stopped.
func New(c config.Cluster, lister k8s.ScaledObjectLister, targets k8s.ScaleTargetReader, hub *Hub, m metrics.MetricsManager) *Backend {
	hub.register(c, m)
	return &Backend{cluster: c, lister: lister, targets: targets, hub: hub}
}

// Discover lists the ScaledObjects with an external trigger addressed to this controller for this cluster.
func (b *Backend) Discover(ctx context.Context) ([]actuator.Workload, error) {
	items, err := b.lister.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list ScaledObjects: %w", err)
	}
	match := b.hub.addressMatch()
	var out []actuator.Workload
	aliases := map[key]key{}
	for i := range items {
		so := &items[i]
		md, ok := b.trigger(so, match)
		if !ok {
			continue
		}
		w := actuator.Workload{
			Namespace:   firstNonEmpty(md["namespace"], so.Namespace),
			Name:        firstNonEmpty(md["workload"], so.Spec.ScaleTargetRef.Name),
			Kind:        scaleTargetKind(so),
			Annotations: so.Annotations,
		}
		aliases[key{so.Namespace, so.Name}] = key{w.Namespace, w.Name}
		out = append(out, w)
	}
	b.hub.setAliases(b.cluster.Env, aliases)
	return out, nil
}

// Replicas reads spec.replicas of the scale target.
func (b *Backend) Replicas(ctx context.Context, w actuator.Workload) (int32, error) {
	if b.targets == nil {
		return 0, actuator.ErrReplicasUnknown
	}
	return b.targets.Replicas(ctx, w.Kind, w.Namespace, w.Name)
}

// Apply publishes the desired state to the hub and its open streams.
func (b *Backend) Apply(_ context.Context, w actuator.Workload, running bool) error {
	b.hub.setRunning(b.cluster.Env, key{w.Namespace, w.Name}, running)
	return nil
}

// Forget drops the published state of w.
func (b *Backend) Forget(_ context.Context, w actuator.Workload) error {
	b.hub.forget(b.cluster.Env, key{w.Namespace, w.Name})
	return nil
}

// trigger returns the metadata of the external trigger of so that targets this cluster.
func (b *Backend) trigger(so *kedav1alpha1.ScaledObject, match string) (map[string]string, bool) {
	for _, t := range so.Spec.Triggers {
		if !externalTriggerTypes[t.Type] {
			continue
		}
		if strings.Contains(t.Metadata["scalerAddress"], match) && t.Metadata["cluster"] == b.cluster.Env {
			return t.Metadata, true
		}
	}
	return nil, false
}

func scaleTargetKind(so *kedav1alpha1.ScaledObject) string {
	if k := so.Spec.ScaleTargetRef.Kind; k != "" {
		return k
	}
	return "Deployment"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
