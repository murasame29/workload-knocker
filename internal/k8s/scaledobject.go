// Package k8s is the low-level layer that calls the K8s API directly.
// Domain logic lives in internal/store; this layer only does simple CRUD/List behind interfaces.
package k8s

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScaledObjectLister lists KEDA ScaledObjects across all namespaces.
type ScaledObjectLister interface {
	List(ctx context.Context) ([]kedav1alpha1.ScaledObject, error)
}

type kedaScaledObjectLister struct {
	client kedaclient.Interface
}

// NewKEDAScaledObjectLister returns a ScaledObjectLister backed by the KEDA clientset.
func NewKEDAScaledObjectLister(client kedaclient.Interface) ScaledObjectLister {
	return &kedaScaledObjectLister{client: client}
}

func (l *kedaScaledObjectLister) List(ctx context.Context) ([]kedav1alpha1.ScaledObject, error) {
	list, err := l.client.KedaV1alpha1().ScaledObjects("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
