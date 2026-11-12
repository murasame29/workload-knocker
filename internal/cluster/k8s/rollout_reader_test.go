package k8s

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func int32p(v int32) *int32 { return &v }

func TestRolloutReader_Replicas(t *testing.T) {
	t.Parallel()

	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "terraria"}}
	c := fake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "terraria", Namespace: "labs"},
			Spec:       appsv1.DeploymentSpec{Replicas: int32p(2), Selector: sel},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "labs"},
			Spec:       appsv1.StatefulSetSpec{Selector: sel},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
		},
	)
	r := NewRolloutReader(c)

	got, err := r.Replicas(context.Background(), "Deployment", "labs", "terraria")
	if err != nil || got != (ReplicaStatus{Desired: 2, Ready: 1, Selector: "app=terraria"}) {
		t.Errorf("Deployment = %+v, %v", got, err)
	}
	got, err = r.Replicas(context.Background(), "StatefulSet", "labs", "db")
	if err != nil || got.Desired != 1 || got.Ready != 1 {
		t.Errorf("StatefulSet (nil replicas defaults to 1) = %+v, %v", got, err)
	}
	if _, err := r.Replicas(context.Background(), "Rollout", "labs", "x"); !errors.Is(err, ErrUnsupportedKind) {
		t.Errorf("Rollout err = %v, want ErrUnsupportedKind", err)
	}
	if _, err := r.Replicas(context.Background(), "Deployment", "labs", "missing"); err == nil {
		t.Error("missing Deployment: want error")
	}
}

func TestRolloutReader_PodIssue(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"app": "terraria"}
	pod := func(name string, st corev1.PodStatus) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "labs", Labels: labels}, Status: st}
	}
	waiting := func(reason string) corev1.PodStatus {
		return corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}}}
	}
	c := fake.NewClientset(
		pod("creating", waiting("ContainerCreating")),
		pod("pull", waiting("ImagePullBackOff")),
		pod("pending", corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}}}),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "labs", Labels: map[string]string{"app": "x"}}, Status: waiting("CrashLoopBackOff")},
	)
	r := NewRolloutReader(c)

	got, err := r.PodIssue(context.Background(), "labs", "app=terraria")
	if err != nil || got != "ImagePullBackOff" {
		t.Errorf("PodIssue = %q, %v; want ImagePullBackOff (sorted first, other app ignored, ContainerCreating skipped)", got, err)
	}
	if got, _ := r.PodIssue(context.Background(), "labs", "app=none"); got != "" {
		t.Errorf("PodIssue without pods = %q, want empty", got)
	}
	if got, _ := r.PodIssue(context.Background(), "labs", ""); got != "" {
		t.Errorf("PodIssue without selector = %q, want empty", got)
	}
}
