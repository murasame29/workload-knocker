package factory

import (
	"testing"

	"k8s.io/client-go/rest"

	"github.com/murasame29/workload-knocker/internal/config"
)

func TestEverySupportedActuatorHasABuilder(t *testing.T) {
	t.Parallel()

	for _, name := range config.ActuatorNames {
		if _, ok := builders[name]; !ok {
			t.Errorf("config.ActuatorNames lists %q but factory has no builder", name)
		}
	}
	if len(builders) != len(config.ActuatorNames) {
		t.Errorf("factory has %d builders, config lists %d actuators", len(builders), len(config.ActuatorNames))
	}
}

func TestNew_DefaultIsKEDAPrometheus(t *testing.T) {
	t.Parallel()

	d, a, err := New(config.Cluster{Env: "stg", Name: "c"}, &rest.Config{Host: "https://example.invalid"}, nil)
	if err != nil || d == nil || a == nil {
		t.Fatalf("New = %v, %v, %v", d, a, err)
	}
}

func TestNew_RejectsUnknownActuator(t *testing.T) {
	t.Parallel()

	if _, _, err := New(config.Cluster{Actuator: "nope"}, &rest.Config{}, nil); err == nil {
		t.Fatal("New succeeded for an unknown actuator")
	}
}

func TestNew_Direct(t *testing.T) {
	t.Parallel()

	d, a, err := New(config.Cluster{Env: "stg", Name: "c", Actuator: config.ActuatorDirect}, &rest.Config{Host: "https://example.invalid"}, nil)
	if err != nil || d == nil || a == nil {
		t.Fatalf("New = %v, %v, %v", d, a, err)
	}
}
