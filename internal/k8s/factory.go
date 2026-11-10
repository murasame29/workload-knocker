package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NewStateRepositoryForConfig builds the ConfigMap StateRepository of the cluster env from a rest.Config.
func NewStateRepositoryForConfig(cfg *rest.Config, controllerNs, env string) (StateRepository, error) {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	return NewConfigMapStateRepository(kc, controllerNs, env), nil
}

// NewClusterRolloutReader builds a RolloutReader from a rest.Config.
func NewClusterRolloutReader(cfg *rest.Config) (RolloutReader, error) {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	return NewRolloutReader(kc), nil
}
