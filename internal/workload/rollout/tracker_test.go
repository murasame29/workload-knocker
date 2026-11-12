package rollout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/cluster/k8s"
)

// fakeClock advances time instead of sleeping.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.now = c.now.Add(d)
	return true
}

// fakeReader serves replica states by poll number (the last one repeats).
type fakeReader struct {
	states []k8s.ReplicaStatus
	err    error
	issue  string
	polls  int
}

func (f *fakeReader) Replicas(context.Context, string, string, string) (k8s.ReplicaStatus, error) {
	if f.err != nil {
		return k8s.ReplicaStatus{}, f.err
	}
	i := min(f.polls, len(f.states)-1)
	f.polls++
	return f.states[i], nil
}

func (f *fakeReader) PodIssue(context.Context, string, string) (string, error) { return f.issue, nil }

type fakeSource map[string]*fakeReader

func (s fakeSource) Reader(_ context.Context, env string) (k8s.RolloutReader, error) {
	r, ok := s[env]
	if !ok {
		return nil, fmt.Errorf("no cluster %s", env)
	}
	return r, nil
}

func rs(desired, ready int) k8s.ReplicaStatus {
	return k8s.ReplicaStatus{Desired: desired, Ready: ready, Selector: "app=x"}
}

func tg(name string, dir Direction) Target {
	return Target{Env: "dev", Namespace: "labs", Name: name, Kind: "Deployment", Direction: dir}
}

func watch(t *Tracker, targets []Target) []Snapshot {
	var got []Snapshot
	t.Watch(context.Background(), targets, func(s Snapshot) { got = append(got, s) })
	return got
}

func states(s Snapshot) string {
	parts := make([]string, len(s))
	for i, it := range s {
		parts[i] = string(it.State)
	}
	return strings.Join(parts, ",")
}

func newTestTracker(src ReaderSource, opts ...Option) *Tracker {
	return NewTracker(src, 5*time.Second, 10*time.Minute, append([]Option{WithClock(&fakeClock{now: time.Unix(0, 0)})}, opts...)...)
}

func TestWatch_StartBecomesStarted(t *testing.T) {
	t.Parallel()

	// KEDA has not scaled yet, then the pod starts, then it is ready.
	src := fakeSource{"dev": {states: []k8s.ReplicaStatus{rs(0, 0), rs(1, 0), rs(1, 1)}}}
	var started []string
	tr := newTestTracker(src, WithOnStarted(func(tg Target) { started = append(started, tg.Name) }))

	got := watch(tr, []Target{tg("terraria", Start)})

	if len(got) != 2 || states(got[0]) != "starting" || states(got[1]) != "started" {
		t.Fatalf("snapshots = %v, want [starting] then [started]", got)
	}
	if len(started) != 1 || started[0] != "terraria" {
		t.Errorf("onStarted calls = %v, want [terraria]", started)
	}
}

func TestWatch_StopBecomesStopped(t *testing.T) {
	t.Parallel()

	src := fakeSource{"dev": {states: []k8s.ReplicaStatus{rs(1, 1), rs(0, 1), rs(0, 0)}}}
	var started int
	tr := newTestTracker(src, WithOnStarted(func(Target) { started++ }))

	got := watch(tr, []Target{tg("terraria", Stop)})

	if len(got) != 2 || states(got[0]) != "stopping" || states(got[1]) != "stopped" {
		t.Fatalf("snapshots = %v, want [stopping] then [stopped]", got)
	}
	if started != 0 {
		t.Errorf("onStarted called %d times for a stop, want 0", started)
	}
}

func TestWatch_OneEditPerChangeAndPerPoll(t *testing.T) {
	t.Parallel()

	// a is ready at poll 2, b at poll 4: exactly two change snapshots after the initial one.
	src := fakeSource{"dev": {states: []k8s.ReplicaStatus{rs(1, 1)}}, "dev2": {states: []k8s.ReplicaStatus{rs(1, 0), rs(1, 0), rs(1, 0), rs(1, 1)}}}
	b := tg("b", Start)
	b.Env = "dev2"

	got := watch(newTestTracker(src), []Target{tg("a", Start), b})

	var seq []string
	for _, s := range got {
		seq = append(seq, states(s))
	}
	if want := "starting,starting|started,starting|started,started"; strings.Join(seq, "|") != want {
		t.Errorf("snapshots = %s, want %s", strings.Join(seq, "|"), want)
	}
}

func TestWatch_NoEditWhileNothingChanges(t *testing.T) {
	t.Parallel()

	// Ready count moves 0/2 -> 1/2 (still starting): no extra snapshot.
	src := fakeSource{"dev": {states: []k8s.ReplicaStatus{rs(2, 0), rs(2, 1), rs(2, 2)}}}

	got := watch(newTestTracker(src), []Target{tg("a", Start)})

	if len(got) != 2 {
		t.Errorf("snapshots = %d, want 2 (initial + started)", len(got))
	}
}

func TestWatch_TimeoutKeepsLastReason(t *testing.T) {
	t.Parallel()

	r := &fakeReader{states: []k8s.ReplicaStatus{rs(1, 0)}, issue: "ImagePullBackOff"}
	var started int
	tr := newTestTracker(fakeSource{"dev": r}, WithOnStarted(func(Target) { started++ }))

	got := watch(tr, []Target{tg("terraria", Start)})

	last := got[len(got)-1]
	if last[0].State != StateTimeout || last[0].Detail != "0/1 ready, ImagePullBackOff" {
		t.Errorf("last = %+v, want timeout with 0/1 ready, ImagePullBackOff", last[0])
	}
	if got := Line(last[0], tr.Timeout()); got != "⚠ terraria not ready after 10m (0/1 ready, ImagePullBackOff)" {
		t.Errorf("line = %q", got)
	}
	if started != 0 {
		t.Errorf("onStarted called on timeout")
	}
	if r.polls < 100 {
		t.Errorf("polls = %d, want about 120 over 10m at 5s", r.polls)
	}
}

func TestWatch_TimeoutBeforeKEDAScaled(t *testing.T) {
	t.Parallel()

	tr := newTestTracker(fakeSource{"dev": {states: []k8s.ReplicaStatus{rs(0, 0)}}})

	got := watch(tr, []Target{tg("a", Start)})

	if d := got[len(got)-1][0].Detail; !strings.Contains(d, "KEDA") {
		t.Errorf("detail = %q, want it to mention KEDA", d)
	}
}

func TestWatch_StopTimeout(t *testing.T) {
	t.Parallel()

	tr := newTestTracker(fakeSource{"dev": {states: []k8s.ReplicaStatus{rs(0, 1)}}})

	got := watch(tr, []Target{tg("a", Stop)})

	if l := Line(got[len(got)-1][0], tr.Timeout()); l != "⚠ a still running after 10m (1/0 ready)" {
		t.Errorf("line = %q", l)
	}
}

func TestWatch_ReadErrorKeepsPollingThenTimesOut(t *testing.T) {
	t.Parallel()

	tr := newTestTracker(fakeSource{"dev": {err: errors.New("forbidden")}})

	got := watch(tr, []Target{tg("a", Start)})

	last := got[len(got)-1][0]
	if last.State != StateTimeout || !strings.Contains(last.Detail, "forbidden") {
		t.Errorf("last = %+v, want timeout carrying the read error", last)
	}
}

func TestWatch_UnknownClusterTimesOut(t *testing.T) {
	t.Parallel()

	got := watch(newTestTracker(fakeSource{}), []Target{tg("a", Start)})

	if last := got[len(got)-1][0]; last.State != StateTimeout || !strings.Contains(last.Detail, "cluster unavailable") {
		t.Errorf("last = %+v", last)
	}
}

func TestWatch_UnsupportedKindIsUntracked(t *testing.T) {
	t.Parallel()

	src := fakeSource{"dev": {err: fmt.Errorf("%w: Rollout", k8s.ErrUnsupportedKind)}}
	x := tg("a", Start)
	x.Kind = "Rollout"

	got := watch(newTestTracker(src), []Target{x})

	if len(got) != 2 || got[1][0].State != StateUntracked {
		t.Fatalf("snapshots = %v, want initial then untracked", got)
	}
	if l := Line(got[1][0], 0); !strings.Contains(l, "not tracked") {
		t.Errorf("line = %q", l)
	}
}

func TestWatch_CancelStopsWithoutFinalSnapshot(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int
	newTestTracker(fakeSource{"dev": {states: []k8s.ReplicaStatus{rs(1, 0)}}}).Watch(ctx, []Target{tg("a", Start)}, func(Snapshot) { calls++ })

	if calls != 1 {
		t.Errorf("onChange calls = %d, want only the initial snapshot", calls)
	}
}

func TestLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		it   Item
		want string
	}{
		{Item{Target: tg("terraria", Start), State: StateStarting}, "⏳ terraria starting..."},
		{Item{Target: tg("terraria", Start), State: StateStarted}, "✅ terraria started"},
		{Item{Target: tg("terraria", Stop), State: StateStopping}, "⏳ terraria stopping..."},
		{Item{Target: tg("terraria", Stop), State: StateStopped}, "⏹ terraria stopped"},
	}
	for _, c := range cases {
		if got := Line(c.it, time.Minute); got != c.want {
			t.Errorf("Line(%v) = %q, want %q", c.it.State, got, c.want)
		}
	}
	if got := FormatDuration(90 * time.Second); got != "1m30s" {
		t.Errorf("FormatDuration(90s) = %q", got)
	}
}
