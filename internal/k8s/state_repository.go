package k8s

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	stateConfigMapName    = "workload-state"
	stateConfigMapDataKey = "state.json"
)

// StateEntry はワークロード状態の永続化単位。
type StateEntry struct {
	Replicas  int    `json:"replicas"`
	StartedBy string `json:"startedBy,omitempty"`
}

// StateRepository は workload state を永続化する抽象。
// 永続先は controller cluster の ConfigMap だが、テストでは in-memory 実装を差し込める。
type StateRepository interface {
	// Load は永続化された state を返す。未初期化なら自動で空を作って空 map を返す。
	Load(ctx context.Context) (map[string]StateEntry, error)
	// Save は state を上書き保存する。冪等。
	Save(ctx context.Context, entries map[string]StateEntry) error
}

// configMapStateRepository は controller cluster 上の ConfigMap に state を永続化する。
type configMapStateRepository struct {
	client    kubernetes.Interface
	namespace string
	name      string
	dataKey   string
}

// NewConfigMapStateRepository は controller cluster の <namespace>/<workload-state> ConfigMap を
// 使う StateRepository 実装を返す。
func NewConfigMapStateRepository(client kubernetes.Interface, namespace string) StateRepository {
	return &configMapStateRepository{
		client:    client,
		namespace: namespace,
		name:      stateConfigMapName,
		dataKey:   stateConfigMapDataKey,
	}
}

func (r *configMapStateRepository) Load(ctx context.Context) (map[string]StateEntry, error) {
	cm, err := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, r.name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			if createErr := r.createEmpty(ctx); createErr != nil {
				return nil, fmt.Errorf("create empty state configmap: %w", createErr)
			}
			return map[string]StateEntry{}, nil
		}
		return nil, fmt.Errorf("get state configmap: %w", err)
	}

	raw, ok := cm.Data[r.dataKey]
	if !ok || raw == "" {
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

	cm, err := r.client.CoreV1().ConfigMaps(r.namespace).Get(ctx, r.name, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("get state configmap for update: %w", err)
		}
		// NotFound: 新規作成
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: r.namespace},
			Data:       map[string]string{r.dataKey: string(data)},
		}
		if _, err := r.client.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create state configmap: %w", err)
		}
		return nil
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[r.dataKey] = string(data)
	if _, err := r.client.CoreV1().ConfigMaps(r.namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update state configmap: %w", err)
	}
	return nil
}

func (r *configMapStateRepository) createEmpty(ctx context.Context) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: r.namespace},
		Data:       map[string]string{r.dataKey: "{}"},
	}
	if _, err := r.client.CoreV1().ConfigMaps(r.namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return err
	}
	return nil
}
