package k8s

import (
	"fmt"

	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NewClusterAccess builds a ScaledObjectLister and StateRepository from a rest.Config.
// The store package gets its K8s API access through this function.
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
