package k8s

import (
	"context"
	"testing"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestScaleTargetReader_Replicas(t *testing.T) {
	t.Parallel()

	kc := fake.NewClientset()
	kc.PrependReactor("get", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		ga := action.(k8stesting.GetAction)
		if ga.GetName() == "missing" {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "deployments"}, "missing")
		}
		return true, &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: 2}}, nil
	})
	r := NewScaleTargetReader(kc)
	ctx := context.Background()

	for _, kind := range []string{"Deployment", "StatefulSet"} {
		name := map[string]string{"Deployment": "d", "StatefulSet": "s"}[kind]
		got, err := r.Replicas(ctx, kind, "ns", name)
		if err != nil || got != 2 {
			t.Errorf("%s: got (%d, %v), want (2, nil)", kind, got, err)
		}
	}
	if _, err := r.Replicas(ctx, "Deployment", "ns", "missing"); err == nil {
		t.Error("missing target: want error")
	}
	if _, err := r.Replicas(ctx, "Rollout", "ns", "d"); err == nil {
		t.Error("unsupported kind: want error")
	}
}
