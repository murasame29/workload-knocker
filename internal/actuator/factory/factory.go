// Package factory builds the actuators selected by config.Cluster.Actuator or Actuators.
package factory

import (
	"fmt"
	"strings"

	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/actuator/direct"
	"github.com/murasame29/workload-knocker/internal/actuator/kedaexternal"
	"github.com/murasame29/workload-knocker/internal/actuator/kedaprometheus"
	"github.com/murasame29/workload-knocker/internal/actuator/multi"
	"github.com/murasame29/workload-knocker/internal/cluster/k8s"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
)

// builder creates the Discoverer and Actuator of one cluster.
type builder func(c config.Cluster, cfg *rest.Config, m metrics.MetricsManager) (actuator.Discoverer, actuator.Actuator, error)

// builders maps every config.ActuatorNames entry to its implementation.
var builders = map[string]builder{
	config.ActuatorKEDAPrometheus: buildKEDAPrometheus,
	config.ActuatorKEDAExternal:   buildKEDAExternal,
	config.ActuatorDirect:         buildDirect,
}

// New builds the Discoverer and Actuator of cluster c from its rest.Config. Several actuators are
// combined so each workload is driven by the one that discovered it.
func New(c config.Cluster, cfg *rest.Config, m metrics.MetricsManager) (actuator.Discoverer, actuator.Actuator, error) {
	names := c.ActuatorTypes()
	members := make([]multi.Member, 0, len(names))
	for _, name := range names {
		b, ok := builders[name]
		if !ok {
			return nil, nil, fmt.Errorf("unsupported actuator %q (supported: %s)", name, strings.Join(config.ActuatorNames, ", "))
		}
		d, a, err := b(c, cfg, m)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		if len(names) == 1 {
			return d, a, nil
		}
		members = append(members, multi.Member{Name: name, Discoverer: d, Actuator: a})
	}
	be := multi.New(c.Env, members...)
	return be, be, nil
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

func buildKEDAExternal(c config.Cluster, cfg *rest.Config, m metrics.MetricsManager) (actuator.Discoverer, actuator.Actuator, error) {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	kk, err := kedaclient.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("keda.NewForConfig: %w", err)
	}
	be := kedaexternal.New(c, k8s.NewKEDAScaledObjectLister(kk), k8s.NewScaleTargetReader(kc), kedaexternal.Shared(), m)
	return be, be, nil
}

func buildDirect(c config.Cluster, cfg *rest.Config, m metrics.MetricsManager) (actuator.Discoverer, actuator.Actuator, error) {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	be := direct.New(c, kc, m, config.GitOpsManagers())
	return be, be, nil
}
