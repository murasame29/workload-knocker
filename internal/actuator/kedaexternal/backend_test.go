package kedaexternal

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
)

var testCluster = config.Cluster{Env: "stg", Name: "stg-cluster"}

const testAddress = "workload-knocker.ctl.svc:9090"

type fakeTargets struct {
	mu       sync.Mutex
	replicas map[string]int32
	broken   map[string]bool
}

func (f *fakeTargets) Replicas(_ context.Context, kind, ns, name string) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := kind + "/" + ns + "/" + name
	if f.broken[k] {
		return 0, errors.New("forbidden")
	}
	return f.replicas[k], nil
}

func scaledObject(name, ns, target string, trigger kedav1alpha1.ScaleTriggers) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: target, Kind: "Deployment"},
			Triggers:       []kedav1alpha1.ScaleTriggers{trigger},
		},
	}
}

func pushTrigger(md map[string]string) kedav1alpha1.ScaleTriggers {
	return kedav1alpha1.ScaleTriggers{Type: "external-push", Metadata: md}
}

type fixture struct {
	kk      *kedafake.Clientset
	targets *fakeTargets
	hub     *Hub
}

func (f *fixture) Manage(w actuator.Workload, replicas int32) {
	so := scaledObject(w.Name, w.Namespace, w.Name, pushTrigger(map[string]string{"scalerAddress": testAddress, "cluster": "stg"}))
	if _, err := f.kk.KedaV1alpha1().ScaledObjects(w.Namespace).Create(context.Background(), so, metav1.CreateOptions{}); err != nil {
		panic(err)
	}
	f.targets.replicas[w.Kind+"/"+w.Namespace+"/"+w.Name] = replicas
}

func (f *fixture) Unmanage(w actuator.Workload) {
	if err := f.kk.KedaV1alpha1().ScaledObjects(w.Namespace).Delete(context.Background(), w.Name, metav1.DeleteOptions{}); err != nil {
		panic(err)
	}
}

func (f *fixture) BreakReplicas(w actuator.Workload) {
	f.targets.broken[w.Kind+"/"+w.Namespace+"/"+w.Name] = true
}

func (f *fixture) Published(w actuator.Workload) (bool, bool) {
	f.hub.mu.Lock()
	defer f.hub.mu.Unlock()
	v, ok := f.hub.clusters["stg"].running[key{w.Namespace, w.Name}]
	return v, ok
}

func TestContract(t *testing.T) {
	t.Parallel()
	actuatortest.Run(t, func(t *testing.T) actuatortest.Harness {
		fx := &fixture{
			kk:      kedafake.NewSimpleClientset(),
			targets: &fakeTargets{replicas: map[string]int32{}, broken: map[string]bool{}},
			hub:     NewHub(),
		}
		be := New(testCluster, k8s.NewKEDAScaledObjectLister(fx.kk), fx.targets, fx.hub, nil)
		return actuatortest.Harness{Discoverer: be, Actuator: be, Fixture: fx}
	})
}

func TestDiscover_Matching(t *testing.T) {
	t.Parallel()

	md := func(address, cluster string) map[string]string {
		return map[string]string{"scalerAddress": address, "cluster": cluster}
	}
	tests := []struct {
		name    string
		trigger kedav1alpha1.ScaleTriggers
		want    bool
	}{
		{"external-push to this controller", pushTrigger(md(testAddress, "stg")), true},
		{"external pull to this controller", kedav1alpha1.ScaleTriggers{Type: "external", Metadata: md(testAddress, "stg")}, true},
		{"another scaler address", pushTrigger(md("other-scaler.ns.svc:9090", "stg")), false},
		{"another cluster", pushTrigger(md(testAddress, "prd")), false},
		{"no cluster", pushTrigger(map[string]string{"scalerAddress": testAddress}), false},
		{"other trigger type", kedav1alpha1.ScaleTriggers{Type: "prometheus", Metadata: md(testAddress, "stg")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kk := kedafake.NewSimpleClientset(scaledObject("so", "ns", "app", tc.trigger))
			be := New(testCluster, k8s.NewKEDAScaledObjectLister(kk), nil, NewHub(), nil)
			got, err := be.Discover(context.Background())
			if err != nil || (len(got) == 1) != tc.want {
				t.Fatalf("Discover = %v, %v, want match=%v", got, err, tc.want)
			}
		})
	}
}

func TestDiscover_WorkloadIdentity(t *testing.T) {
	t.Parallel()

	t.Run("defaults to the ScaledObject namespace and scale target", func(t *testing.T) {
		t.Parallel()
		kk := kedafake.NewSimpleClientset(scaledObject("so", "ns", "app", pushTrigger(map[string]string{"scalerAddress": testAddress, "cluster": "stg"})))
		got, err := New(testCluster, k8s.NewKEDAScaledObjectLister(kk), nil, NewHub(), nil).Discover(context.Background())
		if err != nil || len(got) != 1 || got[0].Namespace != "ns" || got[0].Name != "app" || got[0].Kind != "Deployment" {
			t.Fatalf("Discover = %+v, %v", got, err)
		}
	})

	t.Run("metadata overrides namespace and workload", func(t *testing.T) {
		t.Parallel()
		md := map[string]string{"scalerAddress": testAddress, "cluster": "stg", "namespace": "other", "workload": "api"}
		kk := kedafake.NewSimpleClientset(scaledObject("so", "ns", "app", pushTrigger(md)))
		got, err := New(testCluster, k8s.NewKEDAScaledObjectLister(kk), nil, NewHub(), nil).Discover(context.Background())
		if err != nil || len(got) != 1 || got[0].Namespace != "other" || got[0].Name != "api" {
			t.Fatalf("Discover = %+v, %v", got, err)
		}
	})
}

func TestDiscover_AddressMatchIsConfigurable(t *testing.T) {
	t.Parallel()

	kk := kedafake.NewSimpleClientset(scaledObject("so", "ns", "app", pushTrigger(map[string]string{"scalerAddress": "knocker.example:9090", "cluster": "stg"})))
	hub := NewHub()
	be := New(testCluster, k8s.NewKEDAScaledObjectLister(kk), nil, hub, nil)
	if got, _ := be.Discover(context.Background()); len(got) != 0 {
		t.Fatalf("default match discovered %v", got)
	}
	hub.SetAddressMatch("knocker.example")
	if got, _ := be.Discover(context.Background()); len(got) != 1 {
		t.Fatalf("configured match discovered %v, want 1", got)
	}
}

func TestReplicas_UnknownWithoutReader(t *testing.T) {
	t.Parallel()

	be := New(testCluster, nil, nil, NewHub(), nil)
	if _, err := be.Replicas(context.Background(), actuator.Workload{}); !errors.Is(err, actuator.ErrReplicasUnknown) {
		t.Errorf("Replicas error = %v, want ErrReplicasUnknown", err)
	}
}
