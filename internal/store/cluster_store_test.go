package store

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/murasame29/workload-knocker/internal/actuator/kedaprometheus"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/model"
)

const (
	testControllerNs   = "workload-control"
	testStateConfigMap = "workload-state"
)

// testStateConfigMapKey is the state key of the "stg" env used by most tests.
var testStateConfigMapKey = k8s.StateKey("stg")

// fakeMetrics is a minimal implementation of metrics.MetricsManager.
type fakeMetrics struct {
	mu              sync.Mutex
	activations     map[string]float64 // key: "env/cluster/ns/name"
	deleted         map[string]struct{}
	reachable       map[string]float64
	tokenRefreshes  map[string]int // key: "cluster/result"
	deletedClusters []string
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

func (f *fakeMetrics) ObserveOperation(env, cluster, operation, result string, d time.Duration) {}

func (f *fakeMetrics) IncTokenRefresh(cluster, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.tokenRefreshes[cluster+"/"+result]++
}

func (f *fakeMetrics) DeleteCluster(env, cluster string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureLocked()
	f.deletedClusters = append(f.deletedClusters, env+"/"+cluster)
}

func (f *fakeMetrics) SetGitOpsConflict(string, string, string, string, float64) {}
func (f *fakeMetrics) DeleteGitOpsConflict(string, string, string, string)       {}

func (f *fakeMetrics) IncConfigReload(string, string) {}

func (f *fakeMetrics) IncReconcile(string, string)        {}
func (f *fakeMetrics) IncAutoStop(string, string, string) {}

func (f *fakeMetrics) SetExternalScalerStreams(string, int)   {}
func (f *fakeMetrics) SetReservations(string, string, int)    {}
func (f *fakeMetrics) IncReservationExecution(string, string) {}

func (f *fakeMetrics) Handler() http.Handler { return http.NotFoundHandler() }

// testCluster returns a test Cluster fixture keyed by env. Cluster.Name is "<env>-cluster".
func testCluster(env string) config.Cluster {
	return config.Cluster{
		Env:       env,
		Name:      env + "-cluster",
		Provider:  config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
		Frontends: config.ClusterFrontends{Slack: &config.SlackFrontend{SlashCommand: "/workload-knocker-" + env}},
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
	return kedaStore(c, k8s.NewKEDAScaledObjectLister(kk), k8s.NewConfigMapStateRepository(kc, testControllerNs, c.Env), m, nil)
}

// kedaStore builds a ClusterStore on the keda-prometheus actuator; targets may be nil.
func kedaStore(c config.Cluster, lister k8s.ScaledObjectLister, state k8s.StateRepository, m *fakeMetrics, targets k8s.ScaleTargetReader) *ClusterStore {
	be := kedaprometheus.New(c, lister, targets, m)
	return NewClusterStore(c, be, be, state)
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

	if store.Start("ns-a", "wl-1", "alice", time.Time{}) != nil {
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

	if store.Start("ns-a", "wl-1", "alice", time.Time{}) != nil {
		t.Fatalf("Start: false")
	}
	if store.Stop("ns-a", "wl-1") != nil {
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

	if store.Start("ns-a", "wl-1", "alice", time.Time{}) != nil {
		t.Fatalf("Start wl-1: false")
	}
	if store.Start("ns-a", "wl-2", "bob", time.Time{}) != nil {
		t.Fatalf("Start wl-2: false")
	}

	stopped, _, err := store.StopAll(context.Background(), nil)
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

func TestClusterStore_StopAt_PersistsAcrossRestartAndClearsOnStop(t *testing.T) {
	t.Parallel()

	c := testCluster("stg")
	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))
	stopAt := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

	first := newTestStore(t, c, kc, kk, &fakeMetrics{})
	if err := first.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if first.Start("ns-a", "wl-1", "alice", stopAt) != nil {
		t.Fatal("Start = false")
	}

	second := newTestStore(t, c, kc, kk, &fakeMetrics{})
	if err := second.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap after restart: %v", err)
	}
	if got := second.WorkloadsByNamespace("ns-a")[0].StopAt; !got.Equal(stopAt) {
		t.Fatalf("StopAt after restart = %v, want %v", got, stopAt)
	}

	later := stopAt.Add(time.Hour)
	if !second.SetStopAt("ns-a", "wl-1", later) {
		t.Fatal("SetStopAt on a running workload = false")
	}
	if got := second.WorkloadsByNamespace("ns-a")[0].StopAt; !got.Equal(later) {
		t.Errorf("StopAt after SetStopAt = %v, want %v", got, later)
	}

	if second.Stop("ns-a", "wl-1") != nil {
		t.Fatal("Stop = false")
	}
	if !second.WorkloadsByNamespace("ns-a")[0].StopAt.IsZero() {
		t.Error("Stop must clear StopAt")
	}
	if second.SetStopAt("ns-a", "wl-1", later) {
		t.Error("SetStopAt on a stopped workload = true, want false")
	}
	if _, ok := readStateRaw(t, kc)["ns-a/wl-1"]["stopAt"]; ok {
		t.Error("stopAt must be omitted from the state of a stopped workload")
	}
}

func TestClusterStore_StopAll_ClearsStopAt(t *testing.T) {
	t.Parallel()

	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))
	s := newTestStore(t, testCluster("stg"), kc, kk, &fakeMetrics{})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Start("ns-a", "wl-1", "alice", time.Now().Add(time.Hour))
	if _, _, err := s.StopAll(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !s.WorkloadsByNamespace("ns-a")[0].StopAt.IsZero() {
		t.Error("StopAll must clear StopAt")
	}
}

func TestClusterStore_State_WithoutStopAtStaysBackwardCompatible(t *testing.T) {
	t.Parallel()

	kc := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testStateConfigMap, Namespace: testControllerNs},
		Data:       map[string]string{testStateConfigMapKey: `{"ns-a/wl-1":{"replicas":1,"startedBy":"alice"}}`},
	})
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))
	s := newTestStore(t, testCluster("stg"), kc, kk, &fakeMetrics{})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := s.WorkloadsByNamespace("ns-a")[0]
	if w.Replicas != 1 || !w.StopAt.IsZero() {
		t.Errorf("replicas=%d stopAt=%v, want running without auto-stop", w.Replicas, w.StopAt)
	}
}

func TestClusterStore_DurationAnnotations(t *testing.T) {
	t.Parallel()

	so := makeScaledObject("ns-a", "wl-1")
	so.Annotations = map[string]string{
		"workload-knocker.io/default-duration": "4h",
		"workload-knocker.io/max-duration":     "nonsense",
	}
	kk := kedafake.NewSimpleClientset(so, makeScaledObject("ns-a", "wl-2"))
	s := newTestStore(t, testCluster("stg"), fake.NewClientset(), kk, &fakeMetrics{})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	byName := map[string]*model.Workload{}
	for _, w := range s.WorkloadsByNamespace("ns-a") {
		byName[w.Name] = w
	}
	if d := byName["wl-1"].DefaultDuration; d == nil || *d != 4*time.Hour {
		t.Errorf("wl-1 default = %v, want 4h", d)
	}
	if byName["wl-1"].MaxDuration != 0 {
		t.Errorf("an invalid max annotation must be ignored, got %v", byName["wl-1"].MaxDuration)
	}
	if byName["wl-2"].DefaultDuration != nil {
		t.Errorf("wl-2 default = %v, want unset", byName["wl-2"].DefaultDuration)
	}
}

func TestClusterStore_WorkloadsByNamespace_ReturnsCopies(t *testing.T) {
	t.Parallel()

	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))
	s := newTestStore(t, testCluster("stg"), fake.NewClientset(), kk, &fakeMetrics{})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.WorkloadsByNamespace("ns-a")[0].Replicas = 5
	if got := s.WorkloadsByNamespace("ns-a")[0].Replicas; got != 0 {
		t.Errorf("mutating a returned workload changed the store: replicas=%d", got)
	}
}

// readStateRaw returns the state ConfigMap as generic maps to inspect omitted keys.
func readStateRaw(t *testing.T, kc *fake.Clientset) map[string]map[string]any {
	t.Helper()
	cm, err := kc.CoreV1().ConfigMaps(testControllerNs).Get(context.Background(), testStateConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal([]byte(cm.Data[testStateConfigMapKey]), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClusterStore_StartedAt_PersistsAndClears(t *testing.T) {
	t.Parallel()

	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))
	startedAt := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)

	first := newTestStore(t, testCluster("stg"), kc, kk, &fakeMetrics{})
	first.now = func() time.Time { return startedAt }
	if err := first.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.Start("ns-a", "wl-1", "alice", time.Time{})

	second := newTestStore(t, testCluster("stg"), kc, kk, &fakeMetrics{})
	if err := second.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := second.WorkloadsByNamespace("ns-a")[0].StartedAt; !got.Equal(startedAt) {
		t.Fatalf("StartedAt after restart = %v, want %v", got, startedAt)
	}

	second.SetStopAt("ns-a", "wl-1", startedAt.Add(time.Hour))
	if got := second.WorkloadsByNamespace("ns-a")[0].StartedAt; !got.Equal(startedAt) {
		t.Errorf("SetStopAt must keep StartedAt, got %v", got)
	}

	second.Stop("ns-a", "wl-1")
	if !second.WorkloadsByNamespace("ns-a")[0].StartedAt.IsZero() {
		t.Error("Stop must clear StartedAt")
	}
	if _, ok := readStateRaw(t, kc)["ns-a/wl-1"]["startedAt"]; ok {
		t.Error("startedAt must be omitted from the state of a stopped workload")
	}
}

func TestClusterStore_Bootstrap_LegacyStateWithoutStartedAt(t *testing.T) {
	t.Parallel()

	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testStateConfigMap, Namespace: testControllerNs},
		Data:       map[string]string{testStateConfigMapKey: `{"ns-a/wl-1":{"replicas":1,"startedBy":"carol"}}`},
	}
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))
	s := newTestStore(t, testCluster("stg"), fake.NewClientset(existing), kk, &fakeMetrics{})
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := s.WorkloadsByNamespace("ns-a")[0]
	if w.StartedBy != "carol" || !w.StartedAt.IsZero() {
		t.Errorf("legacy state: StartedBy=%q StartedAt=%v, want carol and zero", w.StartedBy, w.StartedAt)
	}
}
