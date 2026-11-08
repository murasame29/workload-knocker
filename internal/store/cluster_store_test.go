package store

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	nrx "github.com/murasame29/workload-knocker/pkg/newrelic"
)

const (
	testControllerNs      = "workload-control"
	testStateConfigMap    = "workload-state"
	testStateConfigMapKey = "state.json"
)

// fakeMetrics is a minimal implementation of metrics.MetricsManager.
type fakeMetrics struct {
	mu             sync.Mutex
	activations    map[string]float64 // key: "env/cluster/ns/name"
	deleted        map[string]struct{}
	reachable      map[string]float64
	tokenRefreshes map[string]int // key: "cluster/result"
}

func (f *fakeMetrics) ensureLocked() {
	if f.activations == nil {
		f.activations = map[string]float64{}
	}
	if f.deleted == nil {
		f.deleted = map[string]struct{}{}
	}
	if f.reachable == nil {
		f.reachable = map[string]float64{}
	}
	if f.tokenRefreshes == nil {
		f.tokenRefreshes = map[string]int{}
	}
}

func (f *fakeMetrics) SetActivation(env, cluster, namespace, workload string, value float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.activations[env+"/"+cluster+"/"+namespace+"/"+workload] = value
}

func (f *fakeMetrics) DeleteMetric(env, cluster, namespace, workload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.deleted[env+"/"+cluster+"/"+namespace+"/"+workload] = struct{}{}
}

func (f *fakeMetrics) SetReachable(cluster string, value float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.reachable[cluster] = value
}

func (f *fakeMetrics) IncTokenRefresh(cluster, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.tokenRefreshes[cluster+"/"+result]++
}

func (f *fakeMetrics) Handler() http.Handler { return http.NotFoundHandler() }

// testCluster returns a test Cluster fixture keyed by env. Cluster.Name is "<env>-cluster".
func testCluster(env string) config.Cluster {
	return config.Cluster{
		Env:      env,
		Name:     env + "-cluster",
		Provider: config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
		Slack:    config.ClusterSlack{SlashCommand: "/workload-knocker-" + env},
	}
}

// makeScaledObject creates a ScaledObject with an activation trigger.
func makeScaledObject(ns, name string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{
				Name: name,
				Kind: "Deployment",
			},
			Triggers: []kedav1alpha1.ScaleTriggers{
				{
					Type: "prometheus",
					Metadata: map[string]string{
						"query": `workload_knocker_activation{env="stg",namespace="` + ns + `",workload="` + name + `"}`,
					},
				},
			},
		},
	}
}

// newTestStore is a helper that builds a ClusterStore from fake K8s clients.
func newTestStore(t *testing.T, c config.Cluster, kc *fake.Clientset, kk *kedafake.Clientset, m *fakeMetrics) *ClusterStore {
	t.Helper()
	return NewClusterStore(
		c,
		k8s.NewKEDAScaledObjectLister(kk),
		k8s.NewConfigMapStateRepository(kc, testControllerNs),
		m,
		nrx.New(nil),
	)
}

func TestClusterStore_Bootstrap_RegistersScaledObjects(t *testing.T) {
	t.Parallel()

	c := testCluster("stg")
	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(
		makeScaledObject("ns-a", "wl-1"),
		makeScaledObject("ns-a", "wl-2"),
		makeScaledObject("ns-b", "wl-3"),
	)

	store := newTestStore(t, c, kc, kk, &fakeMetrics{})

	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	nss := store.Namespaces()
	if len(nss) != 2 {
		t.Fatalf("Namespaces(): got %d, want 2 (got=%v)", len(nss), nss)
	}

	wls := store.WorkloadsByNamespace("ns-a")
	if len(wls) != 2 {
		t.Fatalf("WorkloadsByNamespace(ns-a): got %d, want 2", len(wls))
	}
	for _, w := range wls {
		if w.Env != "stg" {
			t.Errorf("workload.Cluster: got %q, want %q", w.Env, "stg")
		}
		if w.Namespace != "ns-a" {
			t.Errorf("workload.Namespace: got %q, want %q", w.Namespace, "ns-a")
		}
		if w.Replicas != 0 {
			t.Errorf("workload.Replicas: got %d, want 0 (initial)", w.Replicas)
		}
	}

	wls = store.WorkloadsByNamespace("ns-b")
	if len(wls) != 1 {
		t.Fatalf("WorkloadsByNamespace(ns-b): got %d, want 1", len(wls))
	}
}

// readState JSON-deserializes the state ConfigMap contents and returns them.
func readState(t *testing.T, kc *fake.Clientset) map[string]struct {
	Replicas  int    `json:"replicas"`
	StartedBy string `json:"startedBy,omitempty"`
} {
	t.Helper()
	cm, err := kc.CoreV1().ConfigMaps(testControllerNs).Get(context.Background(), testStateConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get state ConfigMap: %v", err)
	}
	raw := cm.Data[testStateConfigMapKey]
	if raw == "" {
		t.Fatal("state ConfigMap data is empty")
	}
	var out map[string]struct {
		Replicas  int    `json:"replicas"`
		StartedBy string `json:"startedBy,omitempty"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return out
}

func TestClusterStore_Start_UpdatesConfigMap(t *testing.T) {
	t.Parallel()

	c := testCluster("stg")
	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))

	store := newTestStore(t, c, kc, kk, &fakeMetrics{})
	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if !store.Start("ns-a", "wl-1", "alice") {
		t.Fatalf("Start(ns-a, wl-1, alice) = false, want true")
	}

	entries := readState(t, kc)
	entry, ok := entries["ns-a/wl-1"]
	if !ok {
		t.Fatalf("entry ns-a/wl-1 missing in state (got %v)", entries)
	}
	if entry.Replicas != 1 {
		t.Errorf("entry.Replicas: got %d, want 1", entry.Replicas)
	}
	if entry.StartedBy != "alice" {
		t.Errorf("entry.StartedBy: got %q, want %q", entry.StartedBy, "alice")
	}
}

func TestClusterStore_Stop_ClearsReplicas(t *testing.T) {
	t.Parallel()

	c := testCluster("stg")
	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))

	store := newTestStore(t, c, kc, kk, &fakeMetrics{})
	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if !store.Start("ns-a", "wl-1", "alice") {
		t.Fatalf("Start: false")
	}
	if !store.Stop("ns-a", "wl-1") {
		t.Fatalf("Stop: false")
	}

	entries := readState(t, kc)
	entry := entries["ns-a/wl-1"]
	if entry.Replicas != 0 {
		t.Errorf("entry.Replicas after Stop: got %d, want 0", entry.Replicas)
	}
	if entry.StartedBy != "" {
		t.Errorf("entry.StartedBy after Stop: got %q, want empty", entry.StartedBy)
	}

	wls := store.WorkloadsByNamespace("ns-a")
	if len(wls) != 1 {
		t.Fatalf("WorkloadsByNamespace: got %d, want 1", len(wls))
	}
	if wls[0].Replicas != 0 {
		t.Errorf("workload.Replicas after Stop: got %d, want 0", wls[0].Replicas)
	}
}

func TestClusterStore_StopAll_ReturnsCount(t *testing.T) {
	t.Parallel()

	c := testCluster("stg")
	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(
		makeScaledObject("ns-a", "wl-1"),
		makeScaledObject("ns-a", "wl-2"),
		makeScaledObject("ns-b", "wl-3"),
	)

	store := newTestStore(t, c, kc, kk, &fakeMetrics{})
	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if !store.Start("ns-a", "wl-1", "alice") {
		t.Fatalf("Start wl-1: false")
	}
	if !store.Start("ns-a", "wl-2", "bob") {
		t.Fatalf("Start wl-2: false")
	}

	stopped, err := store.StopAll(context.Background())
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if stopped != 2 {
		t.Errorf("StopAll stopped count: got %d, want 2", stopped)
	}

	for _, ns := range []string{"ns-a", "ns-b"} {
		for _, w := range store.WorkloadsByNamespace(ns) {
			if w.Replicas != 0 {
				t.Errorf("after StopAll: %s/%s Replicas=%d, want 0", w.Namespace, w.Name, w.Replicas)
			}
		}
	}

	entries := readState(t, kc)
	for k, e := range entries {
		if e.Replicas != 0 {
			t.Errorf("state entry %s: Replicas=%d, want 0", k, e.Replicas)
		}
	}
}

// TestClusterStore_Bootstrap_WithExistingConfigMap checks restoration when a state ConfigMap already exists.
func TestClusterStore_Bootstrap_WithExistingConfigMap(t *testing.T) {
	t.Parallel()

	c := testCluster("stg")

	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testStateConfigMap,
			Namespace: testControllerNs,
		},
		Data: map[string]string{
			testStateConfigMapKey: `{"ns-a/wl-1":{"replicas":1,"startedBy":"carol"}}`,
		},
	}
	kc := fake.NewClientset(existing)
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))

	store := newTestStore(t, c, kc, kk, &fakeMetrics{})
	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	wls := store.WorkloadsByNamespace("ns-a")
	if len(wls) != 1 {
		t.Fatalf("WorkloadsByNamespace: got %d, want 1", len(wls))
	}
	if wls[0].Replicas != 1 {
		t.Errorf("workload.Replicas after restore: got %d, want 1", wls[0].Replicas)
	}
	if wls[0].StartedBy != "carol" {
		t.Errorf("workload.StartedBy after restore: got %q, want %q", wls[0].StartedBy, "carol")
	}
}
