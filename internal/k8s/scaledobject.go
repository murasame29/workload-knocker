// Package k8s は K8s API を直接叩く低レベル層。ドメインロジックは internal/store に置き、
// この層は単純な CRUD / List だけに責務を限定する。テスト容易性のため interface を公開する。
package k8s

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScaledObjectLister は KEDA ScaledObject を全 namespace で list する抽象。
type ScaledObjectLister interface {
	List(ctx context.Context) ([]kedav1alpha1.ScaledObject, error)
}

type kedaScaledObjectLister struct {
	client kedaclient.Interface
}

// NewKEDAScaledObjectLister は KEDA clientset を使った ScaledObjectLister 実装を返す。
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
