package store

import (
	"context"
	"errors"
	"testing"

	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/murasame29/workload-knocker/internal/k8s"
)

type fakeTargets struct {
	replicas map[string]int32 // key: "kind/ns/name"
	err      map[string]error
	calls    int
}

func (f *fakeTargets) Replicas(_ context.Context, kind, ns, name string) (int32, error) {
	f.calls++
	key := kind + "/" + ns + "/" + name
	if err := f.err[key]; err != nil {
		return 0, err
	}
	return f.replicas[key], nil
}

func newTargetStore(kc *fake.Clientset, kk *kedafake.Clientset, m *fakeMetrics, t k8s.ScaleTargetReader) *ClusterStore {
	return kedaStore(testCluster("stg"), k8s.NewKEDAScaledObjectLister(kk), k8s.NewConfigMapStateRepository(kc, testControllerNs), m, t)
}

func TestClusterStore_InitialState(t *testing.T) {
	t.Parallel()

	targets := &fakeTargets{
		replicas: map[string]int32{"Deployment/ns/running": 2, "Deployment/ns/stopped": 0},
		err:      map[string]error{"Deployment/ns/broken": errors.New("forbidden")},
	}
	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns", "running"), makeScaledObject("ns", "stopped"), makeScaledObject("ns", "broken"))
	m := &fakeMetrics{}
	s := newTargetStore(kc, kk, m, targets)

	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	got := map[string]int{}
	for _, w := range s.WorkloadsByNamespace("ns") {
		got[w.Name] = w.Replicas
	}
	if got["running"] != 1 || got["stopped"] != 0 || got["broken"] != 0 || len(got) != 3 {
		t.Errorf("replicas = %v, want running=1 stopped=0 broken=0", got)
	}
	if v := m.activations["stg/stg-cluster/ns/running"]; v != 1 {
		t.Errorf("activation(running) = %v, want 1", v)
	}
	if v := m.activations["stg/stg-cluster/ns/stopped"]; v != 0 {
		t.Errorf("activation(stopped) = %v, want 0", v)
	}
	if st := readState(t, kc); st["ns/running"].Replicas != 1 {
		t.Errorf("persisted state = %+v, want ns/running replicas=1", st)
	}
}

func TestClusterStore_InitialState_StatefulSetKind(t *testing.T) {
	t.Parallel()

	so := makeScaledObject("ns", "db")
	so.Spec.ScaleTargetRef.Kind = "StatefulSet"
	targets := &fakeTargets{replicas: map[string]int32{"StatefulSet/ns/db": 1}}
	s := newTargetStore(fake.NewClientset(), kedafake.NewSimpleClientset(so), &fakeMetrics{}, targets)

	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if w := s.WorkloadsByNamespace("ns"); len(w) != 1 || w[0].Replicas != 1 {
		t.Errorf("workloads = %+v, want db running", w)
	}
}

func TestClusterStore_Bootstrap_SavedStateWinsOverTarget(t *testing.T) {
	t.Parallel()

	targets := &fakeTargets{replicas: map[string]int32{"Deployment/ns/wl": 3}}
	kc := fake.NewClientset()
	repo := k8s.NewConfigMapStateRepository(kc, testControllerNs)
	if err := repo.Save(context.Background(), map[string]k8s.StateEntry{"ns/wl": {Replicas: 0}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	s := newTargetStore(kc, kedafake.NewSimpleClientset(makeScaledObject("ns", "wl")), &fakeMetrics{}, targets)

	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if w := s.WorkloadsByNamespace("ns"); len(w) != 1 || w[0].Replicas != 0 {
		t.Errorf("workloads = %+v, want saved stopped state", w)
	}
	if targets.calls != 0 {
		t.Errorf("scale target reads = %d, want 0 for a workload with saved state", targets.calls)
	}
}

func TestClusterStore_Reconcile_AddsAndRemovesSeries(t *testing.T) {
	t.Parallel()

	targets := &fakeTargets{replicas: map[string]int32{"Deployment/ns/late": 1}}
	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns", "early"))
	m := &fakeMetrics{}
	s := newTargetStore(kc, kk, m, targets)
	ctx := context.Background()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if _, err := kk.KedaV1alpha1().ScaledObjects("ns").Create(ctx, makeScaledObject("ns", "late"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ScaledObject: %v", err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if v, ok := m.activations["stg/stg-cluster/ns/late"]; !ok || v != 1 {
		t.Errorf("activation(late) = %v (present=%v), want 1", v, ok)
	}
	if st := readState(t, kc); st["ns/late"].Replicas != 1 {
		t.Errorf("persisted state = %+v, want ns/late replicas=1", st)
	}

	if err := kk.KedaV1alpha1().ScaledObjects("ns").Delete(ctx, "early", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete ScaledObject: %v", err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := m.deleted["stg/stg-cluster/ns/early"]; !ok {
		t.Error("series of the deleted ScaledObject was not removed")
	}
}
