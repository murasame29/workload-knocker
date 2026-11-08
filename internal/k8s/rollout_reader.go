package k8s

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrUnsupportedKind is returned for scale target kinds the rollout reader cannot read.
var ErrUnsupportedKind = errors.New("unsupported scale target kind")

// ReplicaStatus is the replica state of a scale target.
type ReplicaStatus struct {
	// Desired is spec.replicas; Ready is status.readyReplicas.
	Desired int
	Ready   int
	// Selector is the label selector of the pods the target owns.
	Selector string
}

// RolloutReader reads the rollout state of scale targets in one cluster.
type RolloutReader interface {
	// Replicas returns the replica state of a Deployment or StatefulSet.
	Replicas(ctx context.Context, kind, namespace, name string) (ReplicaStatus, error)
	// PodIssue returns the most telling waiting or scheduling reason of the pods matching selector, or "".
	PodIssue(ctx context.Context, namespace, selector string) (string, error)
}

type rolloutReader struct{ client kubernetes.Interface }

// NewRolloutReader returns a RolloutReader backed by a clientset.
func NewRolloutReader(client kubernetes.Interface) RolloutReader {
	return &rolloutReader{client: client}
}

func (r *rolloutReader) Replicas(ctx context.Context, kind, namespace, name string) (ReplicaStatus, error) {
	switch kind {
	case "", "Deployment":
		d, err := r.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return ReplicaStatus{}, err
		}
		return replicaStatus(d.Spec.Replicas, d.Status.ReadyReplicas, d.Spec.Selector)
	case "StatefulSet":
		s, err := r.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return ReplicaStatus{}, err
		}
		return replicaStatus(s.Spec.Replicas, s.Status.ReadyReplicas, s.Spec.Selector)
	}
	return ReplicaStatus{}, fmt.Errorf("%w: %s", ErrUnsupportedKind, kind)
}

func replicaStatus(desired *int32, ready int32, sel *metav1.LabelSelector) (ReplicaStatus, error) {
	st := ReplicaStatus{Ready: int(ready)}
	if desired != nil {
		st.Desired = int(*desired)
	} else {
		st.Desired = 1
	}
	if sel != nil {
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return ReplicaStatus{}, fmt.Errorf("invalid pod selector: %w", err)
		}
		st.Selector = s.String()
	}
	return st, nil
}

func (r *rolloutReader) PodIssue(ctx context.Context, namespace, selector string) (string, error) {
	if selector == "" {
		return "", nil
	}
	pods, err := r.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", err
	}
	var reasons []string
	for i := range pods.Items {
		if reason := podIssue(&pods.Items[i]); reason != "" && !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}
	slices.Sort(reasons)
	if len(reasons) == 0 {
		return "", nil
	}
	return reasons[0], nil
}

// podIssue returns a container waiting reason, else the unschedulable reason, else "".
func podIssue(p *corev1.Pod) string {
	statuses := append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" && w.Reason != "ContainerCreating" {
			return w.Reason
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason != "" {
			return c.Reason
		}
	}
	return ""
}
