package store

import (
	"context"
	"sort"
	"testing"
	"time"

	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
)

// newFakeRegistry builds a validated *Config from the given clusters.
func newFakeRegistry(t *testing.T, cs ...config.Cluster) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Controller: config.Controller{Namespace: testControllerNs},
		Clusters:   cs,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return cfg
}

// newTestMultiStore builds a MultiStore by assembling ClusterStores directly (bypassing NewMultiStore).
// A map of fake K8s clientsets per env is injected into each ClusterStore.
func newTestMultiStore(
	t *testing.T,
	reg *config.Config,
	kcs map[string]*fake.Clientset,
	kks map[string]*kedafake.Clientset,
	m *fakeMetrics,
) *MultiStore {
	t.Helper()
	stores := map[string]*ClusterStore{}
	controllerNs := reg.Controller.Namespace
	for _, c := range reg.All() {
		kc, ok := kcs[c.Env]
		if !ok {
			continue
		}
		kk, ok := kks[c.Env]
		if !ok {
			continue
		}
		lister := k8s.NewKEDAScaledObjectLister(kk)
		repo := k8s.NewConfigMapStateRepository(kc, controllerNs, c.Env)
		stores[c.Env] = kedaStore(c, lister, repo, m, nil)
	}
	ms, err := NewMultiStoreFromClusterStores(reg, m, stores, 10*time.Second)
	if err != nil {
		t.Fatalf("NewMultiStoreFromClusterStores: %v", err)
	}
	return ms
}

func TestMultiStore_Clusters(t *testing.T) {
	t.Parallel()

	cA := testCluster("a")
	cB := testCluster("b")
	reg := newFakeRegistry(t, cA, cB)

	kcs := map[string]*fake.Clientset{
		"a": fake.NewClientset(),
		"b": fake.NewClientset(),
	}
	kks := map[string]*kedafake.Clientset{
		"a": kedafake.NewSimpleClientset(),
		"b": kedafake.NewSimpleClientset(),
	}

	ms := newTestMultiStore(t, reg, kcs, kks, &fakeMetrics{})

	clusters := ms.Clusters()
	if len(clusters) != 2 {
		t.Fatalf("Clusters(): got %d, want 2 (got=%v)", len(clusters), clusters)
	}

	envs := []string{clusters[0].Env, clusters[1].Env}
	sort.Strings(envs)
	if envs[0] != "a" || envs[1] != "b" {
		t.Errorf("Clusters() envs: got %v, want [a, b]", envs)
	}
}

func TestMultiStore_Start_ScopedToCluster(t *testing.T) {
	t.Parallel()

	cA := testCluster("a")
	cB := testCluster("b")
	reg := newFakeRegistry(t, cA, cB)

	kcs := map[string]*fake.Clientset{
		"a": fake.NewClientset(),
		"b": fake.NewClientset(),
	}
	kks := map[string]*kedafake.Clientset{
		"a": kedafake.NewSimpleClientset(makeScaledObject("ns-1", "wl-x")),
		"b": kedafake.NewSimpleClientset(makeScaledObject("ns-1", "wl-x")),
	}

	ms := newTestMultiStore(t, reg, kcs, kks, &fakeMetrics{})
	if err := ms.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if ms.Start("a", "ns-1", "wl-x", "alice", time.Time{}) != nil {
		t.Fatalf("Start(a, ns-1, wl-x): false")
	}

	wlsA := ms.WorkloadsByNamespace("a", "ns-1")
	if len(wlsA) != 1 {
		t.Fatalf("cluster a WorkloadsByNamespace: got %d, want 1", len(wlsA))
	}
	if wlsA[0].Replicas != 1 {
		t.Errorf("cluster a wl-x Replicas: got %d, want 1", wlsA[0].Replicas)
	}
	if wlsA[0].StartedBy != "alice" {
		t.Errorf("cluster a wl-x StartedBy: got %q, want %q", wlsA[0].StartedBy, "alice")
	}
	if wlsA[0].Env != "a" {
		t.Errorf("cluster a wl-x Env: got %q, want %q", wlsA[0].Env, "a")
	}

	wlsB := ms.WorkloadsByNamespace("b", "ns-1")
	if len(wlsB) != 1 {
		t.Fatalf("cluster b WorkloadsByNamespace: got %d, want 1", len(wlsB))
	}
	if wlsB[0].Replicas != 0 {
		t.Errorf("cluster b wl-x Replicas after Start(a): got %d, want 0 (cluster border violation)", wlsB[0].Replicas)
	}
	if wlsB[0].StartedBy != "" {
		t.Errorf("cluster b wl-x StartedBy after Start(a): got %q, want empty (cluster border violation)", wlsB[0].StartedBy)
	}
}

// TestMultiStore_Start_UnknownClusterReturnsFalse verifies false is returned for an absent env.
func TestMultiStore_Start_UnknownClusterReturnsFalse(t *testing.T) {
	t.Parallel()

	cA := testCluster("a")
	reg := newFakeRegistry(t, cA)
	kcs := map[string]*fake.Clientset{"a": fake.NewClientset()}
	kks := map[string]*kedafake.Clientset{"a": kedafake.NewSimpleClientset()}
	ms := newTestMultiStore(t, reg, kcs, kks, &fakeMetrics{})

	if ms.Start("nonexistent", "ns", "wl", "alice", time.Time{}) == nil {
		t.Errorf("Start(nonexistent): got true, want false")
	}
	if ms.Stop("nonexistent", "ns", "wl") == nil {
		t.Errorf("Stop(nonexistent): got true, want false")
	}
	if ns := ms.Namespaces("nonexistent"); len(ns) != 0 {
		t.Errorf("Namespaces(nonexistent): got %v, want empty", ns)
	}
	if wls := ms.WorkloadsByNamespace("nonexistent", "any"); len(wls) != 0 {
		t.Errorf("WorkloadsByNamespace(nonexistent): got %v, want empty", wls)
	}
	if err := ms.Reconcile(context.Background(), "nonexistent"); err == nil {
		t.Errorf("Reconcile(nonexistent): got nil, want error")
	}
}
