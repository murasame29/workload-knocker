package k8s

import (
	"fmt"

	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NewClusterAccess は rest.Config から ScaledObjectLister と StateRepository をまとめて生成する。
// 上位レイヤ (store package) はこの関数経由で K8s API への接続を得る。
func NewClusterAccess(cfg *rest.Config, controllerNs string) (ScaledObjectLister, StateRepository, error) {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	kk, err := kedaclient.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("keda.NewForConfig: %w", err)
	}
	return NewKEDAScaledObjectLister(kk), NewConfigMapStateRepository(kc, controllerNs), nil
}
