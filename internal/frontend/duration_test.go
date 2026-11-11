package frontend

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// durStore is an in-memory store that keeps StopAt like the real one.
type durStore struct {
	model.WorkloadStore
	mu sync.Mutex
	ws map[string]*model.Workload
}

func (s *durStore) WorkloadsByNamespace(string, string) []*model.Workload {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Workload
	for _, w := range s.ws {
		c := *w
		out = append(out, &c)
	}
	return out
}

func (s *durStore) Start(_, _, name, _ string, stopAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.ws[name]
	if !ok {
		return model.ErrWorkloadNotFound
	}
	w.Replicas, w.StopAt = 1, stopAt
	return nil
}

func (s *durStore) SetStopAt(_, _, name string, stopAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.ws[name]
	if !ok || w.Replicas == 0 {
		return false
	}
	w.StopAt = stopAt
	return true
}

type auditSink struct{ recs []audit.Record }

func (a *auditSink) Record(_ context.Context, r audit.Record) { a.recs = append(a.recs, r) }

var clock0 = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

func newDurService(ws ...*model.Workload) (*Service, *durStore, *auditSink) {
	st := &durStore{ws: map[string]*model.Workload{}}
	for _, w := range ws {
		st.ws[w.Name] = w
	}
	rec := &auditSink{}
	svc := NewService(st, metrics.New(), 0,
		WithAuthorizer(allowAll), WithRecorder(rec), WithClock(func() time.Time { return clock0 }),
		WithRunDefaults(runlimit.Defaults{Default: 4 * time.Hour, Max: 8 * time.Hour}))
	return svc, st, rec
}

func TestApply_StartSetsStopAtFromDurationOrDefault(t *testing.T) {
	t.Parallel()

	svc, st, rec := newDurService(wl("a", 0), wl("b", 0))
	res := svc.Apply(ApplyRequest{
		Cluster: config.Cluster{Env: "stg", Name: "stg-c"}, Namespace: "labs", Subject: alice,
		ToStart: []string{"a", "b"}, Durations: map[string]time.Duration{"a": 3 * time.Hour},
	})

	if !st.ws["a"].StopAt.Equal(clock0.Add(3*time.Hour)) || !st.ws["b"].StopAt.Equal(clock0.Add(4*time.Hour)) {
		t.Errorf("stopAt a=%v b=%v; want 3h and the 4h default", st.ws["a"].StopAt, st.ws["b"].StopAt)
	}
	if !res.Applied[0].StopAt.Equal(clock0.Add(3 * time.Hour)) {
		t.Errorf("Applied.StopAt = %v", res.Applied[0].StopAt)
	}
	if rec.recs[0].Duration != 3*time.Hour || !rec.recs[0].StopAt.Equal(clock0.Add(3*time.Hour)) {
		t.Errorf("audit record lacks duration/stop time: %+v", rec.recs[0])
	}
}

func TestApply_RejectsDurationAboveMaxAndBelowMin(t *testing.T) {
	t.Parallel()

	svc, st, rec := newDurService(wl("a", 0), wl("b", 0))
	res := svc.Apply(ApplyRequest{
		Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, ToStart: []string{"a", "b"},
		Durations: map[string]time.Duration{"a": 9 * time.Hour, "b": time.Minute},
	})

	if len(res.Applied) != 0 || len(res.Rejected) != 2 {
		t.Fatalf("applied=%v rejected=%v", res.Applied, res.Rejected)
	}
	if msg := res.Rejected[0].Message(); !strings.Contains(msg, "a:") || !strings.Contains(msg, "at most 8h") {
		t.Errorf("message = %q", msg)
	}
	if st.ws["a"].Replicas != 0 || rec.recs[0].Outcome != audit.Failure {
		t.Errorf("a must stay stopped; audit=%+v", rec.recs[0])
	}
}

func TestApply_AnnotationOverridesAndNoDefault(t *testing.T) {
	t.Parallel()

	off := time.Duration(0)
	a := wl("a", 0)
	a.DefaultDuration = &off
	b := wl("b", 0)
	b.MaxDuration = time.Hour
	svc, st, _ := newDurService(a, b)

	svc.Apply(ApplyRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, ToStart: []string{"a", "b"}})
	if !st.ws["a"].StopAt.IsZero() {
		t.Errorf("default-duration 0 must start without auto-stop, got %v", st.ws["a"].StopAt)
	}
	if !st.ws["b"].StopAt.Equal(clock0.Add(time.Hour)) {
		t.Errorf("default must be clamped to the 1h max, got %v", st.ws["b"].StopAt)
	}
}

func TestSchedules_ReportsErrorsAndLimits(t *testing.T) {
	t.Parallel()

	svc, _, _ := newDurService(wl("a", 0))
	got := svc.Schedules("stg", "labs", []string{"a"}, map[string]time.Duration{"a": 24 * time.Hour})
	if got[0].Err == nil || got[0].Default != 4*time.Hour || got[0].Max != 8*time.Hour {
		t.Errorf("schedule = %+v", got[0])
	}
}

func TestExtend_MovesStopAtAndCapsAtMax(t *testing.T) {
	t.Parallel()

	a := wl("a", 1)
	a.StopAt = clock0.Add(30 * time.Minute)
	svc, st, rec := newDurService(a, wl("idle", 0), func() *model.Workload {
		w := wl("noauto", 1)
		return w
	}(), func() *model.Workload {
		w := wl("near", 1)
		w.StopAt = clock0.Add(7*time.Hour + 30*time.Minute)
		return w
	}())

	res := svc.Extend(ExtendRequest{
		Cluster: config.Cluster{Env: "stg", Name: "stg-c"}, Namespace: "labs", Subject: alice,
		Names: []string{"a", "idle", "noauto", "near", "ghost"}, Duration: time.Hour,
	})

	if !res.Extended[0].StopAt.Equal(clock0.Add(90*time.Minute)) || res.Extended[0].Err != nil {
		t.Errorf("a: %+v", res.Extended[0])
	}
	if !st.ws["a"].StopAt.Equal(clock0.Add(90 * time.Minute)) {
		t.Errorf("store stopAt = %v", st.ws["a"].StopAt)
	}
	for i, name := range []string{"idle", "noauto", "near", "ghost"} {
		if res.Extended[i+1].Err == nil {
			t.Errorf("%s: want an error, got %+v", name, res.Extended[i+1])
		}
	}
	if !strings.Contains(res.Extended[3].Err.Error(), "may not exceed 8h") {
		t.Errorf("near: %v", res.Extended[3].Err)
	}
	if rec.recs[0].Event != audit.WorkloadExtend || rec.recs[0].Outcome != audit.Success || rec.recs[0].Duration != time.Hour {
		t.Errorf("audit = %+v", rec.recs[0])
	}
	if rec.recs[1].Outcome != audit.Failure {
		t.Errorf("failed extension must be audited as failure: %+v", rec.recs[1])
	}
}

func TestExtend_AuthorizedLikeStart(t *testing.T) {
	t.Parallel()

	a := wl("a", 1)
	a.StopAt = clock0.Add(time.Hour)
	st := &durStore{ws: map[string]*model.Workload{"a": a}}
	rec := &auditSink{}
	svc := NewService(st, metrics.New(), 0, WithRecorder(rec), WithClock(func() time.Time { return clock0 }))

	res := svc.Extend(ExtendRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, Names: []string{"a"}, Duration: time.Hour})
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != ReasonUnauthorized || len(res.Extended) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if rec.recs[0].Outcome != audit.Denied || rec.recs[0].Event != audit.WorkloadExtend {
		t.Errorf("audit = %+v", rec.recs[0])
	}
	if !st.ws["a"].StopAt.Equal(clock0.Add(time.Hour)) {
		t.Error("a denied extend must not move the stop time")
	}
}

func TestUniform(t *testing.T) {
	t.Parallel()

	if Uniform([]string{"a"}, 0) != nil {
		t.Error("zero duration must yield no overrides")
	}
	if m := Uniform([]string{"a", "b"}, time.Hour); m["a"] != time.Hour || m["b"] != time.Hour {
		t.Errorf("map = %v", m)
	}
}
