package k8s

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// ReservationsKey is the data key of the state ConfigMap that holds the reservations of env.
func ReservationsKey(env string) string { return "reservations." + env + ".json" }

// KeyStore reads and writes one data key of a ConfigMap; "" means absent.
type KeyStore interface {
	Load(ctx context.Context) (string, error)
	Save(ctx context.Context, value string) error
}

// configMapKeyStore keeps one key of the state ConfigMap; a merge patch touches only that key.
type configMapKeyStore struct {
	client    kubernetes.Interface
	namespace string
	name      string
	key       string
}

// NewConfigMapKeyStore returns a KeyStore for key of the <namespace>/workload-state ConfigMap.
func NewConfigMapKeyStore(client kubernetes.Interface, namespace, key string) KeyStore {
	return &configMapKeyStore{client: client, namespace: namespace, name: stateConfigMapName, key: key}
}

func (s *configMapKeyStore) Load(ctx context.Context) (string, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get state configmap: %w", err)
	}
	return cm.Data[s.key], nil
}

func (s *configMapKeyStore) Save(ctx context.Context, value string) error {
	patch, err := json.Marshal(map[string]any{"data": map[string]any{s.key: value}})
	if err != nil {
		return fmt.Errorf("marshal configmap patch: %w", err)
	}
	cms := s.client.CoreV1().ConfigMaps(s.namespace)
	_, err = cms.Patch(ctx, s.name, types.MergePatchType, patch, metav1.PatchOptions{})
	if !k8serrors.IsNotFound(err) {
		if err != nil {
			return fmt.Errorf("patch state configmap: %w", err)
		}
		return nil
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace}, Data: map[string]string{s.key: value}}
	if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("create state configmap: %w", err)
		}
		// Created concurrently by another writer; patch once more.
		if _, err := cms.Patch(ctx, s.name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("patch state configmap: %w", err)
		}
	}
	return nil
}
