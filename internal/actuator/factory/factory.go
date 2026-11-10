// Package factory builds the actuator selected by config.Cluster.Actuator.
package factory

import (
	"fmt"
	"strings"

	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/actuator/kedaprometheus"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/metrics"
)

// builder creates the Discoverer and Actuator of one cluster.
type builder func(c config.Cluster, cfg *rest.Config, m metrics.MetricsManager) (actuator.Discoverer, actuator.Actuator, error)

// builders maps every config.ActuatorNames entry to its implementation.
var builders = map[string]builder{
	config.ActuatorKEDAPrometheus: buildKEDAPrometheus,
}

// New builds the Discoverer and Actuator of cluster c from its rest.Config.
func New(c config.Cluster, cfg *rest.Config, m metrics.MetricsManager) (actuator.Discoverer, actuator.Actuator, error) {
	b, ok := builders[c.ActuatorType()]
	if !ok {
		return nil, nil, fmt.Errorf("unsupported actuator %q (supported: %s)", c.ActuatorType(), strings.Join(config.ActuatorNames, ", "))
	}
	return b(c, cfg, m)
}

func buildKEDAPrometheus(c config.Cluster, cfg *rest.Config, m metrics.MetricsManager) (actuator.Discoverer, actuator.Actuator, error) {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	kk, err := kedaclient.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("keda.NewForConfig: %w", err)
	}
	be := kedaprometheus.New(c, k8s.NewKEDAScaledObjectLister(kk), k8s.NewScaleTargetReader(kc), m)
	return be, be, nil
}
