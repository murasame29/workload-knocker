package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	stateConfigMapName = "workload-state"
	// legacyStateKey is the single key written before state was split per cluster env.
	legacyStateKey = "state.json"
)

// StateKey is the ConfigMap data key that holds the state of the cluster env.
func StateKey(env string) string { return "state." + env + ".json" }

// StateEntry is the persisted unit of workload state.
type StateEntry struct {
	Replicas  int    `json:"replicas"`
	StartedBy string `json:"startedBy,omitempty"`
	// StartedAt is when the run started; absent in state written by older versions.
	StartedAt time.Time `json:"startedAt,omitzero"`
	// StopAt is the auto-stop time; absent in state written by older versions (no auto-stop).
	StopAt time.Time `json:"stopAt,omitzero"`
}

// StateRepository persists workload state.
// It is backed by a ConfigMap on the controller cluster; tests can inject an in-memory one.
type StateRepository interface {
	// Load returns the persisted state, creating an empty one if uninitialized.
	Load(ctx context.Context) (map[string]StateEntry, error)
	// Save overwrites the state. Idempotent.
	Save(ctx context.Context, entries map[string]StateEntry) error
}

// configMapStateRepository persists the state of one cluster env under its own key of a shared
// ConfigMap, so several registry entries pointing at the same cluster do not overwrite each other.
type configMapStateRepository struct {
	client    kubernetes.Interface
	namespace string
	name      string
	dataKey   string
}

// NewConfigMapStateRepository returns a StateRepository that stores the state of env in the
// <namespace>/workload-state ConfigMap under StateKey(env).
func NewConfigMapStateRepository(client kubernetes.Interface, namespace, env string) StateRepository {
	return &configMapStateRepository{
		client:    client,
		namespace: namespace,
		name:      stateConfigMapName,
		dataKey:   StateKey(env),
	}
}

func (r *configMapStateRepository) Load(ctx context.Context) (map[string]StateEntry, error) {
	cm, err := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, r.name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			if createErr := r.createEmpty(ctx); createErr != nil && !k8serrors.IsAlreadyExists(createErr) {
				return nil, fmt.Errorf("create empty state configmap: %w", createErr)
			}
			return map[string]StateEntry{}, nil
		}
		return nil, fmt.Errorf("get state configmap: %w", err)
	}

	raw, ok := cm.Data[r.dataKey]
	if !ok {
		// Migrate from the single key of older versions; the next Save removes it.
		raw = cm.Data[legacyStateKey]
	}
	if raw == "" {
		return map[string]StateEntry{}, nil
	}

	var entries map[string]StateEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}
	return entries, nil
}

func (r *configMapStateRepository) Save(ctx context.Context, entries map[string]StateEntry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// A merge patch touches only this env's key (and drops the legacy key), so writers of other
	// envs never conflict.
	patch, err := json.Marshal(map[string]any{"data": map[string]any{r.dataKey: string(data), legacyStateKey: nil}})
	if err != nil {
		return fmt.Errorf("marshal state patch: %w", err)
	}
	_, err = r.client.CoreV1().ConfigMaps(r.namespace).Patch(ctx, r.name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err == nil {
		return nil
	}
	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("patch state configmap: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: r.namespace},
		Data:       map[string]string{r.dataKey: string(data)},
	}
	if _, err := r.client.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			// Another env created it concurrently; retry the patch once.
			if _, perr := r.client.CoreV1().ConfigMaps(r.namespace).Patch(ctx, r.name, types.MergePatchType, patch, metav1.PatchOptions{}); perr != nil {
				return fmt.Errorf("patch state configmap: %w", perr)
			}
			return nil
		}
		return fmt.Errorf("create state configmap: %w", err)
	}
	return nil
}

func (r *configMapStateRepository) createEmpty(ctx context.Context) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: r.namespace},
		Data:       map[string]string{r.dataKey: "{}"},
	}
	_, err := r.client.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{})
	return err
}
