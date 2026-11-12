package k8s

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ScaleTargetReader reads the desired replicas of a ScaledObject's scale target.
type ScaleTargetReader interface {
	// Replicas returns spec.replicas of the Deployment or StatefulSet (kind) via its scale subresource.
	Replicas(ctx context.Context, kind, namespace, name string) (int32, error)
}

type scaleTargetReader struct {
	client kubernetes.Interface
}

// NewScaleTargetReader returns a ScaleTargetReader backed by the Kubernetes clientset.
func NewScaleTargetReader(client kubernetes.Interface) ScaleTargetReader {
	return &scaleTargetReader{client: client}
}

func (r *scaleTargetReader) Replicas(ctx context.Context, kind, namespace, name string) (int32, error) {
	switch kind {
	case "Deployment":
		sc, err := r.client.AppsV1().Deployments(namespace).GetScale(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		return sc.Spec.Replicas, nil
	case "StatefulSet":
		sc, err := r.client.AppsV1().StatefulSets(namespace).GetScale(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		return sc.Spec.Replicas, nil
	default:
		return 0, fmt.Errorf("unsupported scale target kind %q", kind)
	}
}
