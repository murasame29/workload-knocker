package autostop

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

type fakeStore struct {
	mu        sync.Mutex
	workloads map[string]*model.Workload
	stopped   []string
	failStop  bool
	refuse    bool
}

func (f *fakeStore) setRefuse(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = v
}

func (f *fakeStore) Clusters() []model.ClusterRef {
	return []model.ClusterRef{{Env: "stg", Name: "stg-cluster"}}
}
func (f *fakeStore) Namespaces(string) []string { return []string{"ns"} }
func (f *fakeStore) WorkloadsByNamespace(string, string) []*model.Workload {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.Workload
	for _, w := range f.workloads {
		c := *w
		out = append(out, &c)
	}
	return out
}
func (f *fakeStore) Stop(_, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStop {
		return model.ErrWorkloadNotFound
	}
	if f.refuse {
		return &model.RefusedError{Reason: "replicas is managed by argocd-controller; remove replicas from the manifest"}
	}
	f.stopped = append(f.stopped, name)
	f.workloads[name].Replicas = 0
	f.workloads[name].StopAt = time.Time{}
	return nil
}
func (f *fakeStore) extend(name string, stopAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workloads[name].StopAt = stopAt
}

type recMetrics struct {
	metrics.MetricsManager
	results []string
}

func (r *recMetrics) IncAutoStop(env, cluster, result string) {
	r.results = append(r.results, env+"/"+cluster+"/"+result)
}

type recAudit struct{ recs []audit.Record }

func (r *recAudit) Record(_ context.Context, rec audit.Record) { r.recs = append(r.recs, rec) }

type recReminder struct{ got []string }

func (r *recReminder) StopReminder(_ context.Context, w model.Workload, stopAt time.Time) {
	r.got = append(r.got, w.Name+"@"+stopAt.Format(time.RFC3339))
}

var t0 = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

func running(name string, stopAt time.Time) *model.Workload {
	return &model.Workload{Name: name, Namespace: "ns", Kind: "Deployment", Replicas: 1, StopAt: stopAt}
}

func TestTick_StopsOnlyExpiredWorkloads(t *testing.T) {
	t.Parallel()

	s := &fakeStore{workloads: map[string]*model.Workload{
		"due":     running("due", t0),
		"later":   running("later", t0.Add(time.Hour)),
		"forever": running("forever", time.Time{}),
		"off":     {Name: "off", Namespace: "ns", StopAt: t0.Add(-time.Hour)},
	}}
	m, a := &recMetrics{}, &recAudit{}
	h := New(s, m, 0, WithRecorder(a))

	h.Tick(context.Background(), t0)

	if !slices.Equal(s.stopped, []string{"due"}) {
		t.Fatalf("stopped = %v, want [due]", s.stopped)
	}
	if !slices.Equal(m.results, []string{"stg/stg-cluster/success"}) {
		t.Errorf("metrics = %v", m.results)
	}
	if len(a.recs) != 1 {
		t.Fatalf("audit records = %+v", a.recs)
	}
	r := a.recs[0]
	if r.Event != audit.WorkloadAutoStop || r.Source != audit.SourceScheduler || r.Outcome != audit.Success ||
		r.WorkloadName != "due" || r.Cluster != "stg-cluster" || !r.StopAt.Equal(t0) || r.RequestID == "" {
		t.Errorf("record = %+v", r)
	}

	h.Tick(context.Background(), t0.Add(2*time.Hour))
	if !slices.Equal(s.stopped, []string{"due", "later"}) {
		t.Errorf("after the clock moved: stopped = %v", s.stopped)
	}
}

func TestTick_SurvivesRestartBecauseStateIsPersisted(t *testing.T) {
	t.Parallel()

	s := &fakeStore{workloads: map[string]*model.Workload{"w": running("w", t0.Add(time.Hour))}}
	New(s, &recMetrics{}, 0).Tick(context.Background(), t0)
	if len(s.stopped) != 0 {
		t.Fatal("stopped too early")
	}
	// A new Hook has no memory but reads the same persisted state.
	New(s, &recMetrics{}, 0).Tick(context.Background(), t0.Add(time.Hour+time.Second))
	if !slices.Equal(s.stopped, []string{"w"}) {
		t.Errorf("stopped = %v", s.stopped)
	}
}

func TestTick_RefusedStopIsRetriedButRecordedOnce(t *testing.T) {
	t.Parallel()

	s := &fakeStore{refuse: true, workloads: map[string]*model.Workload{"w": running("w", t0)}}
	m, a := &recMetrics{}, &recAudit{}
	h := New(s, m, 0, WithRecorder(a))

	h.Tick(context.Background(), t0)
	h.Tick(context.Background(), t0.Add(time.Minute))
	if len(a.recs) != 1 || a.recs[0].Outcome != audit.Failure || a.recs[0].Err == nil {
		t.Fatalf("audit records = %+v, want one failure", a.recs)
	}

	s.setRefuse(false)
	h.Tick(context.Background(), t0.Add(2*time.Minute))
	if !slices.Equal(s.stopped, []string{"w"}) || len(a.recs) != 2 || a.recs[1].Outcome != audit.Success {
		t.Errorf("after resolution: stopped = %v, audit = %+v", s.stopped, a.recs)
	}
	if !slices.Equal(m.results, []string{"stg/stg-cluster/error", "stg/stg-cluster/success"}) {
		t.Errorf("metrics = %v", m.results)
	}
}

func TestTick_FailedStopIsCountedAndAudited(t *testing.T) {
	t.Parallel()

	s := &fakeStore{failStop: true, workloads: map[string]*model.Workload{"w": running("w", t0)}}
	m, a := &recMetrics{}, &recAudit{}
	New(s, m, 0, WithRecorder(a)).Tick(context.Background(), t0)

	if !slices.Equal(m.results, []string{"stg/stg-cluster/error"}) || a.recs[0].Outcome != audit.Failure || a.recs[0].Err == nil {
		t.Errorf("metrics=%v audit=%+v", m.results, a.recs)
	}
}

func TestTick_RemindsOncePerStopTime(t *testing.T) {
	t.Parallel()

	stopAt := t0.Add(time.Hour)
	s := &fakeStore{workloads: map[string]*model.Workload{"w": running("w", stopAt)}}
	rem := &recReminder{}
	h := New(s, &recMetrics{}, 15*time.Minute, WithReminder(rem))

	h.Tick(context.Background(), t0)
	h.Tick(context.Background(), stopAt.Add(-16*time.Minute))
	if len(rem.got) != 0 {
		t.Fatalf("reminded before the lead time: %v", rem.got)
	}
	h.Tick(context.Background(), stopAt.Add(-15*time.Minute))
	h.Tick(context.Background(), stopAt.Add(-10*time.Minute))
	if len(rem.got) != 1 {
		t.Fatalf("reminders = %v, want exactly one", rem.got)
	}

	// Extending moves the stop time, which earns a new reminder.
	next := stopAt.Add(time.Hour)
	s.extend("w", next)
	h.Tick(context.Background(), stopAt.Add(-5*time.Minute))
	if len(rem.got) != 1 {
		t.Fatalf("reminded again right after extending: %v", rem.got)
	}
	h.Tick(context.Background(), next.Add(-14*time.Minute))
	if len(rem.got) != 2 {
		t.Errorf("reminders = %v, want one per stop time", rem.got)
	}
}

func TestTick_ReminderDisabledAndNoopDefault(t *testing.T) {
	t.Parallel()

	s := &fakeStore{workloads: map[string]*model.Workload{"w": running("w", t0.Add(time.Minute))}}
	rem := &recReminder{}
	New(s, &recMetrics{}, 0, WithReminder(rem)).Tick(context.Background(), t0)
	if len(rem.got) != 0 {
		t.Errorf("lead 0 must disable reminders: %v", rem.got)
	}
	// Without a receiver the default no-op must not panic.
	New(s, &recMetrics{}, time.Hour).Tick(context.Background(), t0)
}
