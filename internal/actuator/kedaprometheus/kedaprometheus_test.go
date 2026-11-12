package kedaprometheus

import (
	"context"
	"errors"
	"sync"
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/actuator/actuatortest"
	"github.com/murasame29/workload-knocker/internal/cluster/k8s"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
)

var testCluster = config.Cluster{Env: "stg", Name: "stg-cluster"}

// fakeMetrics records the activation series; the other MetricsManager methods are unused.
type fakeMetrics struct {
	metrics.MetricsManager
	mu     sync.Mutex
	series map[string]float64
}

func (f *fakeMetrics) SetActivation(env, cluster, ns, name string, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.series[env+"/"+cluster+"/"+ns+"/"+name] = v
}

func (f *fakeMetrics) DeleteMetric(env, cluster, ns, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.series, env+"/"+cluster+"/"+ns+"/"+name)
}

type fakeTargets struct {
	mu       sync.Mutex
	replicas map[string]int32
	broken   map[string]bool
}

func (f *fakeTargets) Replicas(_ context.Context, kind, ns, name string) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := kind + "/" + ns + "/" + name
	if f.broken[key] {
		return 0, errors.New("forbidden")
	}
	return f.replicas[key], nil
}

type fixture struct {
	kk      *kedafake.Clientset
	targets *fakeTargets
	m       *fakeMetrics
}

func key(w actuator.Workload) string { return w.Kind + "/" + w.Namespace + "/" + w.Name }

func (f *fixture) Manage(w actuator.Workload, replicas int32) {
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: w.Namespace},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: w.Name, Kind: w.Kind},
			Triggers: []kedav1alpha1.ScaleTriggers{{
				Type:     "prometheus",
				Metadata: map[string]string{"query": `workload_knocker_activation{workload="` + w.Name + `"}`},
			}},
		},
	}
	if _, err := f.kk.KedaV1alpha1().ScaledObjects(w.Namespace).Create(context.Background(), so, metav1.CreateOptions{}); err != nil {
		panic(err)
	}
	f.targets.replicas[key(w)] = replicas
}

func (f *fixture) Unmanage(w actuator.Workload) {
	if err := f.kk.KedaV1alpha1().ScaledObjects(w.Namespace).Delete(context.Background(), w.Name, metav1.DeleteOptions{}); err != nil {
		panic(err)
	}
}

func (f *fixture) BreakReplicas(w actuator.Workload) { f.targets.broken[key(w)] = true }

func (f *fixture) Published(w actuator.Workload) (bool, bool) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	v, ok := f.m.series["stg/stg-cluster/"+w.Namespace+"/"+w.Name]
	return v == 1, ok
}

func TestContract(t *testing.T) {
	t.Parallel()
	actuatortest.Run(t, func(t *testing.T) actuatortest.Harness {
		fx := &fixture{
			kk:      kedafake.NewSimpleClientset(),
			targets: &fakeTargets{replicas: map[string]int32{}, broken: map[string]bool{}},
			m:       &fakeMetrics{series: map[string]float64{}},
		}
		be := New(testCluster, k8s.NewKEDAScaledObjectLister(fx.kk), fx.targets, fx.m)
		return actuatortest.Harness{Discoverer: be, Actuator: be, Fixture: fx}
	})
}

func TestDiscover_IgnoresScaledObjectsWithoutActivationTrigger(t *testing.T) {
	t.Parallel()

	other := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns"},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "other"},
			Triggers:       []kedav1alpha1.ScaleTriggers{{Type: "prometheus", Metadata: map[string]string{"query": "up"}}},
		},
	}
	be := New(testCluster, k8s.NewKEDAScaledObjectLister(kedafake.NewSimpleClientset(other)), nil, &fakeMetrics{})

	got, err := be.Discover(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("Discover = %v, %v, want none", got, err)
	}
}

func TestDiscover_DefaultsKindToDeployment(t *testing.T) {
	t.Parallel()

	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: "so", Namespace: "ns", Annotations: map[string]string{"a": "b"}},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "app"},
			Triggers:       []kedav1alpha1.ScaleTriggers{{Type: "prometheus", Metadata: map[string]string{"query": "workload_knocker_activation"}}},
		},
	}
	be := New(testCluster, k8s.NewKEDAScaledObjectLister(kedafake.NewSimpleClientset(so)), nil, &fakeMetrics{})

	got, err := be.Discover(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Discover = %v, %v", got, err)
	}
	if got[0].Name != "app" || got[0].Kind != "Deployment" || got[0].Annotations["a"] != "b" {
		t.Errorf("Discover[0] = %+v", got[0])
	}
}

func TestReplicas_UnknownWithoutReader(t *testing.T) {
	t.Parallel()

	be := New(testCluster, nil, nil, &fakeMetrics{})
	if _, err := be.Replicas(context.Background(), actuator.Workload{}); !errors.Is(err, actuator.ErrReplicasUnknown) {
		t.Errorf("Replicas error = %v, want ErrReplicasUnknown", err)
	}
}
