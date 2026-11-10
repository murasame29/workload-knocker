package k8s

import (
	"context"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testNs = "workload-control"

func getStateCM(t *testing.T, kc *fake.Clientset) *corev1.ConfigMap {
	t.Helper()
	cm, err := kc.CoreV1().ConfigMaps(testNs).Get(context.Background(), stateConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get state configmap: %v", err)
	}
	return cm
}

func TestStateRepository_EnvsShareConfigMapWithoutOverwriting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kc := fake.NewClientset()
	a := NewConfigMapStateRepository(kc, testNs, "flowers")
	b := NewConfigMapStateRepository(kc, testNs, "flowers-direct")

	if err := a.Save(ctx, map[string]StateEntry{"terraria/terraria": {Replicas: 1}}); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := b.Save(ctx, map[string]StateEntry{"terraria/tommy-world": {Replicas: 1}}); err != nil {
		t.Fatalf("save b: %v", err)
	}
	if err := a.Save(ctx, map[string]StateEntry{"terraria/terraria": {Replicas: 0}}); err != nil {
		t.Fatalf("save a again: %v", err)
	}

	gotA, err := a.Load(ctx)
	if err != nil || len(gotA) != 1 || gotA["terraria/terraria"].Replicas != 0 {
		t.Errorf("a.Load = %v, %v", gotA, err)
	}
	gotB, err := b.Load(ctx)
	if err != nil || len(gotB) != 1 || gotB["terraria/tommy-world"].Replicas != 1 {
		t.Errorf("b.Load = %v, %v; another env's save must not overwrite it", gotB, err)
	}
	cm := getStateCM(t, kc)
	if _, ok := cm.Data[StateKey("flowers")]; !ok {
		t.Errorf("missing key %s: %v", StateKey("flowers"), cm.Data)
	}
	if _, ok := cm.Data[StateKey("flowers-direct")]; !ok {
		t.Errorf("missing key %s: %v", StateKey("flowers-direct"), cm.Data)
	}
}

func TestStateRepository_MigratesLegacyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kc := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: stateConfigMapName, Namespace: testNs},
		Data:       map[string]string{legacyStateKey: `{"ns/app":{"replicas":1,"startedBy":"alice"}}`},
	})
	r := NewConfigMapStateRepository(kc, testNs, "stg")

	got, err := r.Load(ctx)
	if err != nil || got["ns/app"].Replicas != 1 || got["ns/app"].StartedBy != "alice" {
		t.Fatalf("Load from legacy key = %v, %v", got, err)
	}
	if err := r.Save(ctx, got); err != nil {
		t.Fatalf("save: %v", err)
	}
	cm := getStateCM(t, kc)
	if _, ok := cm.Data[legacyStateKey]; ok {
		t.Errorf("legacy key kept after save: %v", cm.Data)
	}
	if cm.Data[StateKey("stg")] == "" {
		t.Errorf("env key not written: %v", cm.Data)
	}
}

func TestStateRepository_EnvKeyWinsOverLegacy(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: stateConfigMapName, Namespace: testNs},
		Data: map[string]string{
			legacyStateKey:  `{"ns/app":{"replicas":1}}`,
			StateKey("stg"): `{"ns/app":{"replicas":0}}`,
		},
	})
	got, err := NewConfigMapStateRepository(kc, testNs, "stg").Load(context.Background())
	if err != nil || got["ns/app"].Replicas != 0 {
		t.Errorf("Load = %v, %v; want the env key", got, err)
	}
}

func TestStateRepository_CreatesConfigMapOnLoadAndSave(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	kc := fake.NewClientset()
	got, err := NewConfigMapStateRepository(kc, testNs, "stg").Load(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("Load on a missing configmap = %v, %v", got, err)
	}
	if cm := getStateCM(t, kc); cm.Data[StateKey("stg")] != "{}" {
		t.Errorf("empty state not created: %v", cm.Data)
	}

	kc2 := fake.NewClientset()
	if err := NewConfigMapStateRepository(kc2, testNs, "stg").Save(ctx, map[string]StateEntry{"ns/app": {Replicas: 1}}); err != nil {
		t.Fatalf("Save on a missing configmap: %v", err)
	}
	if cm := getStateCM(t, kc2); cm.Data[StateKey("stg")] == "" {
		t.Errorf("state not created on save: %v", cm.Data)
	}
}

func TestStateRepository_ConcurrentEnvSaves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kc := fake.NewClientset()
	envs := []string{"a", "b", "c", "d"}
	var wg sync.WaitGroup
	for _, env := range envs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := NewConfigMapStateRepository(kc, testNs, env)
			for i := range 20 {
				if err := r.Save(ctx, map[string]StateEntry{"ns/" + env: {Replicas: i % 2}}); err != nil {
					t.Errorf("save %s: %v", env, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	cm := getStateCM(t, kc)
	for _, env := range envs {
		if cm.Data[StateKey(env)] == "" {
			t.Errorf("env %s lost its state: %v", env, cm.Data)
		}
	}
}
