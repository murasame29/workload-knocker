// Package direct is an actuator that needs neither KEDA nor Prometheus: it discovers annotated
// Deployments and StatefulSets and writes only their scale subresource.
package direct

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
)

// Annotations that opt a workload in and set its running replicas.
const (
	AnnotationEnabled  = "workload-knocker.io/enabled"
	AnnotationReplicas = "workload-knocker.io/replicas"
)

const (
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
)

// ConflictError is returned by Apply when a GitOps manager owns spec.replicas of the workload.
type ConflictError struct {
	Manager string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("replicas is managed by %s; remove replicas from the manifest", e.Manager)
}

// Refused marks the conflict as an actuator.Refusal shown to the user.
func (*ConflictError) Refused() {}

// Backend implements actuator.Discoverer and actuator.Actuator for one cluster.
type Backend struct {
	cluster  config.Cluster
	client   kubernetes.Interface
	metrics  metrics.MetricsManager
	managers map[string]struct{}

	mu     sync.Mutex
	warned map[string]string // workload key -> last skip reason, to warn once per change
}

var (
	_ actuator.Discoverer = (*Backend)(nil)
	_ actuator.Actuator   = (*Backend)(nil)
)

// New returns a Backend. gitopsManagers are the field managers whose ownership of spec.replicas blocks Apply.
func New(c config.Cluster, client kubernetes.Interface, m metrics.MetricsManager, gitopsManagers []string) *Backend {
	set := make(map[string]struct{}, len(gitopsManagers))
	for _, g := range gitopsManagers {
		set[g] = struct{}{}
	}
	return &Backend{cluster: c, client: client, metrics: m, managers: set, warned: map[string]string{}}
}

// object is the part of a Deployment or StatefulSet the actuator reads.
type object struct {
	meta metav1.ObjectMeta
	kind string
}

// Discover lists annotated Deployments and StatefulSets, skipping invalid and HPA-targeted ones.
func (b *Backend) Discover(ctx context.Context) ([]actuator.Workload, error) {
	deployments, err := b.client.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Deployments: %w", err)
	}
	statefulSets, err := b.client.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list StatefulSets: %w", err)
	}
	var objs []object
	for i := range deployments.Items {
		objs = append(objs, object{meta: deployments.Items[i].ObjectMeta, kind: kindDeployment})
	}
	for i := range statefulSets.Items {
		objs = append(objs, object{meta: statefulSets.Items[i].ObjectMeta, kind: kindStatefulSet})
	}

	var enabled []object
	for _, o := range objs {
		if o.meta.Annotations[AnnotationEnabled] == "true" {
			enabled = append(enabled, o)
		}
	}
	hpas := map[string]string{}
	if len(enabled) > 0 {
		if hpas, err = b.hpaTargets(ctx, metav1.NamespaceAll); err != nil {
			return nil, err
		}
	}

	var out []actuator.Workload
	seen := map[string]struct{}{}
	for _, o := range enabled {
		key := o.meta.Namespace + "/" + o.kind + "/" + o.meta.Name
		seen[key] = struct{}{}
		if reason := skipReason(o, hpas); reason != "" {
			b.warnSkipped(key, o, reason)
			continue
		}
		b.clearWarned(key)
		out = append(out, actuator.Workload{Namespace: o.meta.Namespace, Name: o.meta.Name, Kind: o.kind, Annotations: o.meta.Annotations})
	}
	b.pruneWarned(seen)
	return out, nil
}

// Replicas reads spec.replicas of the scale subresource.
func (b *Backend) Replicas(ctx context.Context, w actuator.Workload) (int32, error) {
	return b.getScale(ctx, w)
}

// Apply scales w to its running replicas (running) or 0 through the scale subresource.
// Workloads that are not (or no longer) managed are left untouched.
func (b *Backend) Apply(ctx context.Context, w actuator.Workload, running bool) error {
	o, err := b.get(ctx, w)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s %s/%s: %w", w.Kind, w.Namespace, w.Name, err)
	}
	if o.meta.Annotations[AnnotationEnabled] != "true" {
		return nil
	}
	hpas, err := b.hpaTargets(ctx, w.Namespace)
	if err != nil {
		return err
	}
	// Discover skips these too; this covers a change made after the last discovery.
	if reason := skipReason(o, hpas); reason != "" {
		return &actuator.RefusedError{Reason: reason}
	}
	target, _ := parseReplicas(o.meta.Annotations)

	if owner := b.gitopsOwner(o.meta.ManagedFields); owner != "" {
		b.metrics.SetGitOpsConflict(b.cluster.Env, b.cluster.Name, w.Namespace, w.Name, 1)
		slog.Warn("replicas is owned by a GitOps manager, workload not scaled", slog.String("cluster.name", b.cluster.Name), slog.String("cluster.env", b.cluster.Env), slog.String("namespace", w.Namespace), slog.String("workload", w.Name), slog.String("manager", owner))
		return &ConflictError{Manager: owner}
	}
	b.metrics.SetGitOpsConflict(b.cluster.Env, b.cluster.Name, w.Namespace, w.Name, 0)

	if !running {
		target = 0
	}
	sc, err := b.scale(ctx, w)
	if err != nil {
		return fmt.Errorf("get scale of %s %s/%s: %w", w.Kind, w.Namespace, w.Name, err)
	}
	if sc.Spec.Replicas == target {
		return nil
	}
	if err := b.setScale(ctx, w, target); err != nil {
		return fmt.Errorf("update scale of %s %s/%s: %w", w.Kind, w.Namespace, w.Name, err)
	}
	return nil
}

// Forget drops the GitOps conflict series of w.
func (b *Backend) Forget(_ context.Context, w actuator.Workload) error {
	b.metrics.DeleteGitOpsConflict(b.cluster.Env, b.cluster.Name, w.Namespace, w.Name)
	return nil
}

func (b *Backend) get(ctx context.Context, w actuator.Workload) (object, error) {
	switch w.Kind {
	case kindDeployment:
		d, err := b.client.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return object{}, err
		}
		return object{meta: d.ObjectMeta, kind: w.Kind}, nil
	case kindStatefulSet:
		s, err := b.client.AppsV1().StatefulSets(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return object{}, err
		}
		return object{meta: s.ObjectMeta, kind: w.Kind}, nil
	default:
		return object{}, fmt.Errorf("unsupported kind %q", w.Kind)
	}
}

func (b *Backend) getScale(ctx context.Context, w actuator.Workload) (int32, error) {
	sc, err := b.scale(ctx, w)
	if err != nil {
		return 0, err
	}
	return sc.Spec.Replicas, nil
}

func (b *Backend) scale(ctx context.Context, w actuator.Workload) (*autoscalingv1.Scale, error) {
	switch w.Kind {
	case kindDeployment:
		return b.client.AppsV1().Deployments(w.Namespace).GetScale(ctx, w.Name, metav1.GetOptions{})
	case kindStatefulSet:
		return b.client.AppsV1().StatefulSets(w.Namespace).GetScale(ctx, w.Name, metav1.GetOptions{})
	default:
		return nil, fmt.Errorf("unsupported kind %q", w.Kind)
	}
}

// setScale writes replicas through the scale subresource only.
func (b *Backend) setScale(ctx context.Context, w actuator.Workload, replicas int32) error {
	sc, err := b.scale(ctx, w)
	if err != nil {
		return err
	}
	sc.Spec.Replicas = replicas
	if w.Kind == kindDeployment {
		_, err = b.client.AppsV1().Deployments(w.Namespace).UpdateScale(ctx, w.Name, sc, metav1.UpdateOptions{})
	} else {
		_, err = b.client.AppsV1().StatefulSets(w.Namespace).UpdateScale(ctx, w.Name, sc, metav1.UpdateOptions{})
	}
	return err
}

// hpaTargets maps kind/name of every HPA scale target in namespace to the HPA name.
func (b *Backend) hpaTargets(ctx context.Context, namespace string) (map[string]string, error) {
	list, err := b.client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list HorizontalPodAutoscalers: %w", err)
	}
	out := make(map[string]string, len(list.Items))
	for _, h := range list.Items {
		ref := h.Spec.ScaleTargetRef
		out[h.Namespace+"/"+hpaKey(ref.Kind, ref.Name)] = h.Name
	}
	return out, nil
}

func hpaKey(kind, name string) string { return kind + "/" + name }

// gitopsOwner returns the GitOps manager that owns spec.replicas, or "".
func (b *Backend) gitopsOwner(fields []metav1.ManagedFieldsEntry) string {
	for _, f := range fields {
		if _, ok := b.managers[f.Manager]; !ok || f.FieldsV1 == nil || f.Subresource != "" {
			continue
		}
		if ownsReplicas(f.FieldsV1.GetRawBytes()) {
			return f.Manager
		}
	}
	return ""
}

func ownsReplicas(raw []byte) bool {
	var fields struct {
		Spec map[string]json.RawMessage `json:"f:spec"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	_, ok := fields.Spec["f:replicas"]
	return ok
}

// runningReplicas parses the replicas annotation: absent means 1, otherwise a positive integer.
func runningReplicas(annotations map[string]string) (int32, error) {
	v, ok := annotations[AnnotationReplicas]
	if !ok {
		return 1, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("annotation %s must be a positive integer, got %q", AnnotationReplicas, v)
	}
	return int32(n), nil
}

func parseReplicas(annotations map[string]string) (int32, bool) {
	n, err := runningReplicas(annotations)
	return n, err == nil
}

func skipReason(o object, hpas map[string]string) string {
	if _, err := runningReplicas(o.meta.Annotations); err != nil {
		return err.Error()
	}
	if hpa, ok := hpas[o.meta.Namespace+"/"+hpaKey(o.kind, o.meta.Name)]; ok {
		return fmt.Sprintf("targeted by HorizontalPodAutoscaler %s, which cannot scale to 0", hpa)
	}
	return ""
}

func (b *Backend) warnSkipped(key string, o object, reason string) {
	b.mu.Lock()
	changed := b.warned[key] != reason
	b.warned[key] = reason
	b.mu.Unlock()
	if changed {
		slog.Warn("workload skipped by the direct actuator", slog.String("cluster.name", b.cluster.Name), slog.String("cluster.env", b.cluster.Env), slog.String("namespace", o.meta.Namespace), slog.String("workload", o.meta.Name), slog.String("kind", o.kind), slog.String("reason", reason))
	}
}

func (b *Backend) clearWarned(key string) {
	b.mu.Lock()
	delete(b.warned, key)
	b.mu.Unlock()
}

func (b *Backend) pruneWarned(seen map[string]struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.warned {
		if _, ok := seen[key]; !ok {
			delete(b.warned, key)
		}
	}
}
