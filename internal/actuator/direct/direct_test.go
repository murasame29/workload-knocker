package direct

import (
	"context"
	"errors"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/actuator/actuatortest"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
)

var testCluster = config.Cluster{Env: "stg", Name: "stg-cluster"}

var defaultManagers = []string{"argocd-controller", "kustomize-controller", "helm-controller"}

type fakeMetrics struct {
	metrics.MetricsManager
	mu        sync.Mutex
	conflicts map[string]float64
}

func newFakeMetrics() *fakeMetrics { return &fakeMetrics{conflicts: map[string]float64{}} }

func (f *fakeMetrics) SetGitOpsConflict(env, cluster, ns, name string, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conflicts[env+"/"+cluster+"/"+ns+"/"+name] = v
}

func (f *fakeMetrics) DeleteGitOpsConflict(env, cluster, ns, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.conflicts, env+"/"+cluster+"/"+ns+"/"+name)
}

func (f *fakeMetrics) conflict(ns, name string) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.conflicts["stg/stg-cluster/"+ns+"/"+name]
	return v, ok
}

// env is a fake cluster whose scale subresources are backed by a replicas map.
type env struct {
	kc       *fake.Clientset
	m        *fakeMetrics
	be       *Backend
	mu       sync.Mutex
	replicas map[string]int32
	broken   map[string]bool
	fields   map[string][]metav1.ManagedFieldsEntry
	updates  []string // scale subresource writes
	writes   []string // actions that wrote anything but the scale subresource
}

func key(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

func kindOf(resource string) string {
	if resource == "statefulsets" {
		return kindStatefulSet
	}
	return kindDeployment
}

func newEnv(managers []string) *env {
	e := &env{kc: fake.NewClientset(), m: newFakeMetrics(), replicas: map[string]int32{}, broken: map[string]bool{}, fields: map[string][]metav1.ManagedFieldsEntry{}}
	e.kc.PrependReactor("get", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "" {
			return e.getWithManagedFields(a.(k8stesting.GetAction))
		}
		ga := a.(k8stesting.GetAction)
		k := key(kindOf(a.GetResource().Resource), ga.GetNamespace(), ga.GetName())
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.broken[k] {
			return true, nil, errors.New("forbidden")
		}
		return true, &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ga.GetNamespace()}, Spec: autoscalingv1.ScaleSpec{Replicas: e.replicas[k]}}, nil
	})
	e.kc.PrependReactor("update", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ua := a.(k8stesting.UpdateAction)
		e.mu.Lock()
		defer e.mu.Unlock()
		if a.GetSubresource() != "scale" {
			e.writes = append(e.writes, "update "+a.GetResource().Resource)
			return false, nil, nil
		}
		sc := ua.GetObject().(*autoscalingv1.Scale)
		k := key(kindOf(a.GetResource().Resource), ua.GetNamespace(), sc.Name)
		e.updates = append(e.updates, k)
		e.replicas[k] = sc.Spec.Replicas
		return true, sc, nil
	})
	e.kc.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		e.mu.Lock()
		e.writes = append(e.writes, "patch "+a.GetResource().Resource)
		e.mu.Unlock()
		return false, nil, nil
	})
	e.be = New(testCluster, e.kc, e.m, managers)
	return e
}

// getWithManagedFields serves workloads with the managedFields set by add; the fake tracker computes its own.
func (e *env) getWithManagedFields(ga k8stesting.GetAction) (bool, runtime.Object, error) {
	obj, err := e.kc.Tracker().Get(ga.GetResource(), ga.GetNamespace(), ga.GetName())
	if err != nil {
		return true, nil, err
	}
	obj = obj.DeepCopyObject()
	e.mu.Lock()
	defer e.mu.Unlock()
	fields := e.fields[key(kindOf(ga.GetResource().Resource), ga.GetNamespace(), ga.GetName())]
	switch o := obj.(type) {
	case *appsv1.Deployment:
		o.ManagedFields = fields
	case *appsv1.StatefulSet:
		o.ManagedFields = fields
	}
	return true, obj, nil
}

func (e *env) add(kind, ns, name string, annotations map[string]string, replicas int32, fields ...metav1.ManagedFieldsEntry) {
	meta := metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations}
	e.fields[key(kind, ns, name)] = fields
	var err error
	switch kind {
	case kindStatefulSet:
		_, err = e.kc.AppsV1().StatefulSets(ns).Create(context.Background(), &appsv1.StatefulSet{ObjectMeta: meta}, metav1.CreateOptions{})
	default:
		_, err = e.kc.AppsV1().Deployments(ns).Create(context.Background(), &appsv1.Deployment{ObjectMeta: meta}, metav1.CreateOptions{})
	}
	if err != nil {
		panic(err)
	}
	e.replicas[key(kind, ns, name)] = replicas
}

func (e *env) addHPA(ns, name, kind, target string) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: kind, Name: target}},
	}
	if _, err := e.kc.AutoscalingV2().HorizontalPodAutoscalers(ns).Create(context.Background(), hpa, metav1.CreateOptions{}); err != nil {
		panic(err)
	}
}

func (e *env) replicasOf(kind, ns, name string) int32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.replicas[key(kind, ns, name)]
}

func enabled(extra ...string) map[string]string {
	a := map[string]string{AnnotationEnabled: "true"}
	for i := 0; i+1 < len(extra); i += 2 {
		a[extra[i]] = extra[i+1]
	}
	return a
}

func fieldsV1(raw string) *metav1.FieldsV1 { return metav1.NewFieldsV1(raw) }

func owned(manager, op string, subresource string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager:     manager,
		Operation:   metav1.ManagedFieldsOperationType(op),
		Subresource: subresource,
		FieldsV1:    fieldsV1(`{"f:spec":{"f:replicas":{},"f:selector":{}}}`),
	}
}

func notOwning(manager string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager:   manager,
		Operation: metav1.ManagedFieldsOperationUpdate,
		FieldsV1:  fieldsV1(`{"f:spec":{"f:selector":{}},"f:metadata":{"f:labels":{}}}`),
	}
}

// contractFixture adapts env to actuatortest.Fixture.
type contractFixture struct{ *env }

func (f contractFixture) Manage(w actuator.Workload, replicas int32) {
	f.add(w.Kind, w.Namespace, w.Name, enabled(), replicas)
}

func (f contractFixture) Unmanage(w actuator.Workload) {
	var err error
	if w.Kind == kindStatefulSet {
		err = f.kc.AppsV1().StatefulSets(w.Namespace).Delete(context.Background(), w.Name, metav1.DeleteOptions{})
	} else {
		err = f.kc.AppsV1().Deployments(w.Namespace).Delete(context.Background(), w.Name, metav1.DeleteOptions{})
	}
	if err != nil {
		panic(err)
	}
}

func (f contractFixture) BreakReplicas(w actuator.Workload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broken[key(w.Kind, w.Namespace, w.Name)] = true
}

// Published reports whether the replicas are above zero; the state Apply publishes and Forget
// releases is the conflict series.
func (f contractFixture) Published(w actuator.Workload) (bool, bool) {
	_, ok := f.m.conflict(w.Namespace, w.Name)
	return f.replicasOf(w.Kind, w.Namespace, w.Name) > 0, ok
}

func TestContract(t *testing.T) {
	t.Parallel()
	actuatortest.Run(t, func(*testing.T) actuatortest.Harness {
		e := newEnv(defaultManagers)
		return actuatortest.Harness{Discoverer: e.be, Actuator: e.be, Fixture: contractFixture{e}}
	})
}

func TestDiscover_FiltersByAnnotation(t *testing.T) {
	t.Parallel()
	e := newEnv(defaultManagers)
	e.add(kindDeployment, "a", "on", enabled(AnnotationReplicas, "3", "workload-knocker.io/ready-message", "hi"), 0)
	e.add(kindStatefulSet, "b", "db", enabled(), 0)
	e.add(kindDeployment, "a", "off", map[string]string{AnnotationEnabled: "false"}, 0)
	e.add(kindDeployment, "a", "plain", nil, 0)

	got, err := e.be.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a/on": kindDeployment, "b/db": kindStatefulSet}
	if len(got) != len(want) {
		t.Fatalf("Discover = %+v, want %v", got, want)
	}
	for _, w := range got {
		if want[w.Namespace+"/"+w.Name] != w.Kind {
			t.Errorf("unexpected workload %+v", w)
		}
		if w.Name == "on" && w.Annotations["workload-knocker.io/ready-message"] != "hi" {
			t.Errorf("annotations not passed through: %v", w.Annotations)
		}
	}
}

func TestDiscover_SkipsInvalidReplicasAnnotation(t *testing.T) {
	t.Parallel()
	e := newEnv(defaultManagers)
	for name, v := range map[string]string{"zero": "0", "neg": "-1", "text": "two", "float": "1.5", "huge": "99999999999"} {
		e.add(kindDeployment, "ns", name, enabled(AnnotationReplicas, v), 0)
	}
	e.add(kindDeployment, "ns", "ok", enabled(AnnotationReplicas, "2"), 0)

	got, err := e.be.Discover(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("Discover = %+v, %v, want only ok", got, err)
	}
}

func TestDiscover_SkipsHPATargets(t *testing.T) {
	t.Parallel()
	e := newEnv(defaultManagers)
	e.add(kindDeployment, "ns", "scaled", enabled(), 2)
	e.add(kindDeployment, "ns", "free", enabled(), 0)
	e.add(kindDeployment, "other", "scaled", enabled(), 0)
	e.addHPA("ns", "scaled-hpa", kindDeployment, "scaled")

	got, err := e.be.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, w := range got {
		names[w.Namespace+"/"+w.Name] = true
	}
	if len(names) != 2 || !names["ns/free"] || !names["other/scaled"] {
		t.Errorf("Discover = %v, want ns/free and other/scaled", names)
	}
}

func TestApply_PatchesOnlyTheScaleSubresource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(defaultManagers)
	e.add(kindDeployment, "ns", "web", enabled(AnnotationReplicas, "3"), 0)
	e.add(kindStatefulSet, "ns", "db", enabled(), 0)

	for _, w := range []actuator.Workload{{Namespace: "ns", Name: "web", Kind: kindDeployment}, {Namespace: "ns", Name: "db", Kind: kindStatefulSet}} {
		if err := e.be.Apply(ctx, w, true); err != nil {
			t.Fatalf("Apply start %s: %v", w.Name, err)
		}
	}
	if got := e.replicasOf(kindDeployment, "ns", "web"); got != 3 {
		t.Errorf("web replicas = %d, want 3", got)
	}
	if got := e.replicasOf(kindStatefulSet, "ns", "db"); got != 1 {
		t.Errorf("db replicas = %d, want 1 (default)", got)
	}
	if err := e.be.Apply(ctx, actuator.Workload{Namespace: "ns", Name: "web", Kind: kindDeployment}, false); err != nil {
		t.Fatal(err)
	}
	if got := e.replicasOf(kindDeployment, "ns", "web"); got != 0 {
		t.Errorf("web replicas after stop = %d, want 0", got)
	}
	if len(e.writes) != 0 {
		t.Errorf("workload objects were written: %v", e.writes)
	}
	if len(e.updates) != 3 {
		t.Errorf("patches = %v, want 3 scale writes", e.updates)
	}
}

func TestApply_SkipsPatchWhenAlreadyAtTarget(t *testing.T) {
	t.Parallel()
	e := newEnv(defaultManagers)
	e.add(kindDeployment, "ns", "web", enabled(), 1)
	w := actuator.Workload{Namespace: "ns", Name: "web", Kind: kindDeployment}
	if err := e.be.Apply(context.Background(), w, true); err != nil {
		t.Fatal(err)
	}
	if len(e.updates) != 0 {
		t.Errorf("patches = %v, want none", e.updates)
	}
}

func TestApply_GitOpsGuard(t *testing.T) {
	t.Parallel()
	w := actuator.Workload{Namespace: "ns", Name: "web", Kind: kindDeployment}
	tests := []struct {
		name         string
		fields       []metav1.ManagedFieldsEntry
		wantConflict string
	}{
		{"argocd applies replicas", []metav1.ManagedFieldsEntry{owned("argocd-controller", "Apply", "")}, "argocd-controller"},
		{"kustomize updates replicas", []metav1.ManagedFieldsEntry{owned("kustomize-controller", "Update", "")}, "kustomize-controller"},
		{"argocd without replicas", []metav1.ManagedFieldsEntry{notOwning("argocd-controller")}, ""},
		{"kube-controller-manager owns replicas", []metav1.ManagedFieldsEntry{owned("kube-controller-manager", "Update", "")}, ""},
		{"KEDA HPA manager owns replicas", []metav1.ManagedFieldsEntry{owned("horizontal-pod-autoscaler", "Update", "scale")}, ""},
		{"GitOps scale subresource entry", []metav1.ManagedFieldsEntry{owned("argocd-controller", "Update", "scale")}, ""},
		{"no managed fields", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(defaultManagers)
			e.add(kindDeployment, "ns", "web", enabled(), 0, tt.fields...)
			err := e.be.Apply(context.Background(), w, true)
			v, published := e.m.conflict("ns", "web")
			if tt.wantConflict == "" {
				if err != nil || e.replicasOf(kindDeployment, "ns", "web") != 1 || !published || v != 0 {
					t.Fatalf("Apply = %v, replicas %d, gauge %v/%v, want scaled with gauge 0", err, e.replicasOf(kindDeployment, "ns", "web"), v, published)
				}
				return
			}
			var ce *ConflictError
			if !errors.As(err, &ce) || ce.Manager != tt.wantConflict {
				t.Fatalf("Apply error = %v, want ConflictError for %s", err, tt.wantConflict)
			}
			if _, ok := actuator.AsRefusal(err); !ok {
				t.Errorf("ConflictError is not a refusal: %v", err)
			}
			if e.replicasOf(kindDeployment, "ns", "web") != 0 || len(e.updates) != 0 {
				t.Errorf("workload was scaled despite the conflict: %v", e.updates)
			}
			if v != 1 {
				t.Errorf("conflict gauge = %v, want 1", v)
			}
		})
	}
}

func TestApply_GitOpsConflictResolves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(defaultManagers)
	e.add(kindDeployment, "ns", "web", enabled(), 0, owned("argocd-controller", "Apply", ""))
	w := actuator.Workload{Namespace: "ns", Name: "web", Kind: kindDeployment}
	if err := e.be.Apply(ctx, w, true); err == nil {
		t.Fatal("Apply succeeded under a conflict")
	}

	e.mu.Lock()
	delete(e.fields, key(kindDeployment, "ns", "web"))
	e.mu.Unlock()
	if err := e.be.Apply(ctx, w, true); err != nil {
		t.Fatalf("Apply after resolution: %v", err)
	}
	if v, _ := e.m.conflict("ns", "web"); v != 0 {
		t.Errorf("conflict gauge = %v, want 0", v)
	}
	if err := e.be.Forget(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.m.conflict("ns", "web"); ok {
		t.Error("conflict series still published after Forget")
	}
}

func TestApply_CustomManagerList(t *testing.T) {
	t.Parallel()
	e := newEnv([]string{"flux"})
	e.add(kindDeployment, "ns", "a", enabled(), 0, owned("flux", "Apply", ""))
	e.add(kindDeployment, "ns", "b", enabled(), 0, owned("argocd-controller", "Apply", ""))
	if err := e.be.Apply(context.Background(), actuator.Workload{Namespace: "ns", Name: "a", Kind: kindDeployment}, true); err == nil {
		t.Error("flux-owned replicas was scaled")
	}
	if err := e.be.Apply(context.Background(), actuator.Workload{Namespace: "ns", Name: "b", Kind: kindDeployment}, true); err != nil {
		t.Errorf("argocd is not in the custom list, got %v", err)
	}
}

func TestApply_LeavesUnmanagedWorkloadsAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(defaultManagers)
	e.add(kindDeployment, "ns", "unannotated", nil, 3)

	for _, name := range []string{"unannotated", "gone"} {
		if err := e.be.Apply(ctx, actuator.Workload{Namespace: "ns", Name: name, Kind: kindDeployment}, false); err != nil {
			t.Errorf("Apply(%s) = %v, want nil", name, err)
		}
	}
	if len(e.updates) != 0 {
		t.Errorf("unmanaged workloads were patched: %v", e.updates)
	}
}

func TestApply_RefusesSkippedWorkloads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(defaultManagers)
	e.add(kindDeployment, "ns", "hpa", enabled(), 3)
	e.add(kindDeployment, "ns", "invalid", enabled(AnnotationReplicas, "0"), 3)
	e.addHPA("ns", "h", kindDeployment, "hpa")

	want := map[string]string{
		"hpa":     "targeted by HorizontalPodAutoscaler h, which cannot scale to 0",
		"invalid": `annotation workload-knocker.io/replicas must be a positive integer, got "0"`,
	}
	for name, reason := range want {
		err := e.be.Apply(ctx, actuator.Workload{Namespace: "ns", Name: name, Kind: kindDeployment}, false)
		if r, ok := actuator.AsRefusal(err); !ok || r.Error() != reason {
			t.Errorf("Apply(%s) = %v, want refusal %q", name, err, reason)
		}
	}
	if len(e.updates) != 0 {
		t.Errorf("refused workloads were patched: %v", e.updates)
	}
}

func TestApply_RejectsUnsupportedKind(t *testing.T) {
	t.Parallel()
	e := newEnv(defaultManagers)
	if err := e.be.Apply(context.Background(), actuator.Workload{Namespace: "ns", Name: "x", Kind: "Rollout"}, true); err == nil {
		t.Error("Apply succeeded for an unsupported kind")
	}
	if _, err := e.be.Replicas(context.Background(), actuator.Workload{Namespace: "ns", Name: "x", Kind: "Rollout"}); err == nil {
		t.Error("Replicas succeeded for an unsupported kind")
	}
}
