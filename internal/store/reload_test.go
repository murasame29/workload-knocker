package store

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
)

// fakeBuilder builds ClusterStores over fake clientsets; scaled objects, build and bootstrap
// failures are configured per env.
type fakeBuilder struct {
	m         *fakeMetrics
	objects   map[string][][2]string // env -> [namespace, name]
	buildErr  map[string]error
	bootError map[string]bool
	builds    []string
	mu        sync.Mutex
}

func (f *fakeBuilder) factory() ClusterFactory {
	return func(_ context.Context, c config.Cluster, ns string) (*ClusterStore, error) {
		f.mu.Lock()
		f.builds = append(f.builds, c.Env)
		f.mu.Unlock()
		if err := f.buildErr[c.Env]; err != nil {
			return nil, err
		}
		var objs []runtime.Object
		for _, o := range f.objects[c.Env] {
			objs = append(objs, makeScaledObject(o[0], o[1]))
		}
		kk := kedafake.NewSimpleClientset(objs...)
		if f.bootError[c.Env] {
			kk.PrependReactor("list", "scaledobjects", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("api unreachable")
			})
		}
		return kedaStore(c, k8s.NewKEDAScaledObjectLister(kk), k8s.NewConfigMapStateRepository(fake.NewClientset(), ns, c.Env), f.m, nil), nil
	}
}

func newReloadFixture(t *testing.T, objects map[string][][2]string, envs ...string) (*MultiStore, *fakeBuilder, *fakeMetrics) {
	t.Helper()
	m := &fakeMetrics{}
	fb := &fakeBuilder{m: m, objects: objects, buildErr: map[string]error{}, bootError: map[string]bool{}}
	var cs []config.Cluster
	for _, e := range envs {
		cs = append(cs, testCluster(e))
	}
	reg := newFakeRegistry(t, cs...)
	stores := map[string]*ClusterStore{}
	for _, c := range reg.All() {
		s, err := fb.factory()(context.Background(), c, testControllerNs)
		if err != nil {
			t.Fatal(err)
		}
		stores[c.Env] = s
	}
	ms, err := NewMultiStoreFromClusterStores(reg, m, stores, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	fb.builds = nil
	return ms, fb, m
}

func TestReload_AddsCluster(t *testing.T) {
	t.Parallel()

	objs := map[string][][2]string{"a": {{"ns", "w1"}}, "b": {{"ns", "w2"}}}
	ms, fb, _ := newReloadFixture(t, objs, "a")
	before, _ := ms.storeFor("a")

	res, err := ms.Reload(context.Background(), newFakeRegistry(t, testCluster("a"), testCluster("b")), fb.factory())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []string{"b"}) || len(res.Removed)+len(res.Changed)+len(res.Kept) != 0 {
		t.Errorf("result = %+v, want only b added", res)
	}
	if after, _ := ms.storeFor("a"); after != before {
		t.Error("unchanged cluster a must keep its store")
	}
	if got := ms.Namespaces("b"); !slices.Equal(got, []string{"ns"}) {
		t.Errorf("Namespaces(b) = %v", got)
	}
	if !slices.Equal(fb.builds, []string{"b"}) {
		t.Errorf("builds = %v, want only b", fb.builds)
	}
	if len(ms.Clusters()) != 2 {
		t.Errorf("Clusters = %v", ms.Clusters())
	}
}

func TestReload_RemovesClusterAndItsSeries(t *testing.T) {
	t.Parallel()

	ms, fb, m := newReloadFixture(t, map[string][][2]string{"a": {{"ns", "w"}}, "b": {{"ns", "w"}}}, "a", "b")

	res, err := ms.Reload(context.Background(), newFakeRegistry(t, testCluster("a")), fb.factory())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Removed, []string{"b"}) {
		t.Errorf("Removed = %v", res.Removed)
	}
	if ms.Namespaces("b") != nil || ms.Start("b", "ns", "w", "u", time.Time{}) == nil {
		t.Error("removed cluster must no longer be served")
	}
	if !slices.Equal(m.deletedClusters, []string{"b/b-cluster"}) {
		t.Errorf("DeleteCluster calls = %v, want b/b-cluster", m.deletedClusters)
	}
	if len(fb.builds) != 0 {
		t.Errorf("builds = %v, want none", fb.builds)
	}
}

func TestReload_ChangedClusterRebuildsAndDropsStaleSeries(t *testing.T) {
	t.Parallel()

	objs := map[string][][2]string{"a": {{"ns", "keep"}, {"ns", "gone"}}}
	ms, fb, m := newReloadFixture(t, objs, "a")
	before, _ := ms.storeFor("a")

	fb.objects["a"] = [][2]string{{"ns", "keep"}}
	changed := testCluster("a")
	changed.Provider.EKS = &config.ProviderEKS{Region: "us-east-1"}
	res, err := ms.Reload(context.Background(), newFakeRegistry(t, changed), fb.factory())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Changed, []string{"a"}) {
		t.Errorf("Changed = %v", res.Changed)
	}
	if after, _ := ms.storeFor("a"); after == before {
		t.Error("changed cluster must get a new store")
	}
	if _, stale := m.deleted["a/a-cluster/ns/gone"]; !stale {
		t.Errorf("series of the vanished workload must be deleted, got %v", m.deleted)
	}
	if _, wrong := m.deleted["a/a-cluster/ns/keep"]; wrong {
		t.Error("series of a surviving workload must stay")
	}
}

func TestReload_RenamedClusterDropsOldNameSeries(t *testing.T) {
	t.Parallel()

	ms, fb, m := newReloadFixture(t, map[string][][2]string{"a": {{"ns", "w"}}}, "a")
	renamed := testCluster("a")
	renamed.Name = "a-renamed"
	if _, err := ms.Reload(context.Background(), newFakeRegistry(t, renamed), fb.factory()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.deletedClusters, []string{"a/a-cluster"}) {
		t.Errorf("DeleteCluster calls = %v, want the old name only", m.deletedClusters)
	}
}

func TestReload_ChangedClusterBootstrapFailureKeepsOldStore(t *testing.T) {
	t.Parallel()

	ms, fb, _ := newReloadFixture(t, map[string][][2]string{"a": {{"ns", "w"}}}, "a")
	before, _ := ms.storeFor("a")
	fb.bootError["a"] = true

	changed := testCluster("a")
	changed.Provider.EKS = &config.ProviderEKS{Region: "us-east-1"}
	res, err := ms.Reload(context.Background(), newFakeRegistry(t, changed), fb.factory())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Kept, []string{"a"}) || len(res.Changed) != 0 {
		t.Errorf("result = %+v, want a kept", res)
	}
	if after, _ := ms.storeFor("a"); after != before {
		t.Error("old store must be kept when the new one cannot bootstrap")
	}
	if ms.Start("a", "ns", "w", "u", time.Time{}) != nil {
		t.Error("old store must still serve")
	}
}

func TestReload_AddedClusterFailures(t *testing.T) {
	t.Parallel()

	ms, fb, m := newReloadFixture(t, map[string][][2]string{"a": {{"ns", "w"}}}, "a")
	fb.buildErr["b"] = errors.New("no credentials")
	fb.bootError["c"] = true

	res, err := ms.Reload(context.Background(), newFakeRegistry(t, testCluster("a"), testCluster("b"), testCluster("c")), fb.factory())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []string{"c"}) {
		t.Errorf("Added = %v, want only c (b could not be built)", res.Added)
	}
	if _, ok := ms.storeFor("b"); ok {
		t.Error("cluster b has no store")
	}
	if _, ok := ms.storeFor("c"); !ok {
		t.Error("cluster c keeps its store with reachable=0, like at startup")
	}
	if m.reachable["c-cluster"] != 0 || m.reachable["b-cluster"] != 0 {
		t.Errorf("reachable = %v, want 0 for b and c", m.reachable)
	}
}

func TestReload_FrontendOnlyChangeKeepsStores(t *testing.T) {
	t.Parallel()

	ms, fb, _ := newReloadFixture(t, map[string][][2]string{"a": {{"ns", "w"}}}, "a")
	c := testCluster("a")
	c.Frontends.Slack = &config.SlackFrontend{SlashCommand: "/new-command"}

	res, err := ms.Reload(context.Background(), newFakeRegistry(t, c), fb.factory())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Empty() || len(fb.builds) != 0 {
		t.Errorf("result = %+v builds = %v, want no store churn", res, fb.builds)
	}
}

func TestReload_ConcurrentRequestsDuringSwap(t *testing.T) {
	t.Parallel()

	objs := map[string][][2]string{"a": {{"ns", "w"}}, "b": {{"ns", "w"}}}
	ms, fb, _ := newReloadFixture(t, objs, "a")

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				ms.Namespaces("a")
				ms.Start("a", "ns", "w", "u", time.Time{})
				ms.Clusters()
				ms.StopAll(context.Background(), nil)
			}
		}()
	}
	for i := range 20 {
		cs := []config.Cluster{testCluster("a")}
		if i%2 == 0 {
			cs = append(cs, testCluster("b"))
		}
		if _, err := ms.Reload(context.Background(), newFakeRegistry(t, cs...), fb.factory()); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	wg.Wait()
}

func TestStopAll_FilterSkipsRunningWorkloads(t *testing.T) {
	t.Parallel()

	objs := map[string][][2]string{"a": {{"ns", "mine"}, {"ns", "theirs"}, {"ns", "idle"}}}
	ms, _, _ := newReloadFixture(t, objs, "a")
	ms.Start("a", "ns", "mine", "u", time.Time{})
	ms.Start("a", "ns", "theirs", "u", time.Time{})

	res := ms.StopAll(context.Background(), func(env, ns, name string) bool { return name != "theirs" && name != "idle" })

	got := res.PerCluster["a"]
	if res.TotalStopped != 1 || got.Stopped != 1 || !slices.Equal(got.Skipped, []string{"ns/theirs"}) {
		t.Errorf("result = %+v, want mine stopped and only the running theirs skipped", res)
	}
	for _, w := range ms.WorkloadsByNamespace("a", "ns") {
		want := 0
		if w.Name == "theirs" {
			want = 1
		}
		if w.Replicas != want {
			t.Errorf("%s replicas = %d, want %d", w.Name, w.Replicas, want)
		}
	}
}

func TestReload_ExplicitDefaultActuatorKeepsStores(t *testing.T) {
	t.Parallel()

	ms, fb, _ := newReloadFixture(t, map[string][][2]string{"a": {{"ns", "w"}}}, "a")
	c := testCluster("a")
	c.Actuator = config.ActuatorKEDAPrometheus

	res, err := ms.Reload(context.Background(), newFakeRegistry(t, c), fb.factory())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Empty() || len(fb.builds) != 0 {
		t.Errorf("result = %+v builds = %v, want no store churn", res, fb.builds)
	}
}
