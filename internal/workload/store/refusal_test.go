package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/actuator/kedaprometheus"
	"github.com/murasame29/workload-knocker/internal/cluster/k8s"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

const conflictReason = "replicas is managed by argocd-controller; remove replicas from the manifest"

// refusingActuator refuses Apply while refuse is set and delegates otherwise.
type refusingActuator struct {
	actuator.Actuator
	mu     sync.Mutex
	refuse bool
}

func (r *refusingActuator) set(refuse bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuse = refuse
}

func (r *refusingActuator) Apply(ctx context.Context, w actuator.Workload, running bool) error {
	r.mu.Lock()
	refuse := r.refuse
	r.mu.Unlock()
	if refuse {
		return &actuator.RefusedError{Reason: conflictReason}
	}
	return r.Actuator.Apply(ctx, w, running)
}

func refusingStore(t *testing.T) (*ClusterStore, *refusingActuator, *fake.Clientset) {
	t.Helper()
	c := testCluster("stg")
	kc := fake.NewClientset()
	kk := kedafake.NewSimpleClientset(makeScaledObject("ns-a", "wl-1"))
	be := kedaprometheus.New(c, k8s.NewKEDAScaledObjectLister(kk), nil, &fakeMetrics{})
	act := &refusingActuator{Actuator: be}
	s := NewClusterStore(c, be, act, k8s.NewConfigMapStateRepository(kc, testControllerNs, c.Env))
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return s, act, kc
}

func TestClusterStore_StartRefusedKeepsStopped(t *testing.T) {
	t.Parallel()
	s, act, kc := refusingStore(t)
	act.set(true)

	err := s.Start("ns-a", "wl-1", "alice", time.Now().Add(time.Hour))
	r, ok := model.AsRefused(err)
	if !ok || r.Reason != conflictReason {
		t.Fatalf("Start = %v, want RefusedError %q", err, conflictReason)
	}
	w := s.WorkloadsByNamespace("ns-a")[0]
	if w.Replicas != 0 || w.StartedBy != "" || !w.StopAt.IsZero() {
		t.Errorf("workload after refused start = %+v, want stopped", *w)
	}
	if e := readState(t, kc)["ns-a/wl-1"]; e.Replicas != 0 || e.StartedBy != "" {
		t.Errorf("persisted state after refused start = %+v, want stopped", e)
	}
}

func TestClusterStore_StopRefusedKeepsRunning(t *testing.T) {
	t.Parallel()
	s, act, kc := refusingStore(t)
	stopAt := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := s.Start("ns-a", "wl-1", "alice", stopAt); err != nil {
		t.Fatalf("Start: %v", err)
	}
	act.set(true)

	if _, ok := model.AsRefused(s.Stop("ns-a", "wl-1")); !ok {
		t.Fatal("Stop was not refused")
	}
	w := s.WorkloadsByNamespace("ns-a")[0]
	if w.Replicas != 1 || w.StartedBy != "alice" || !w.StopAt.Equal(stopAt) {
		t.Errorf("workload after refused stop = %+v, want still running by alice", *w)
	}
	if e := readState(t, kc)["ns-a/wl-1"]; e.Replicas != 1 || e.StartedBy != "alice" {
		t.Errorf("persisted state after refused stop = %+v, want running", e)
	}

	act.set(false)
	if err := s.Stop("ns-a", "wl-1"); err != nil {
		t.Fatalf("Stop after resolution: %v", err)
	}
}

func TestClusterStore_UnknownWorkloadIsNotFound(t *testing.T) {
	t.Parallel()
	s, _, _ := refusingStore(t)
	if err := s.Start("ns-a", "nope", "alice", time.Time{}); !errors.Is(err, model.ErrWorkloadNotFound) {
		t.Errorf("Start = %v, want ErrWorkloadNotFound", err)
	}
	if err := s.Stop("ns-a", "nope"); !errors.Is(err, model.ErrWorkloadNotFound) {
		t.Errorf("Stop = %v, want ErrWorkloadNotFound", err)
	}
}
