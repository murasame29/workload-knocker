package reconcile

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/observability/metrics"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

type recMetrics struct {
	metrics.MetricsManager
	mu     sync.Mutex
	counts map[string]int
}

func (r *recMetrics) IncReconcile(cluster, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[cluster+"/"+result]++
}

func (r *recMetrics) get(k string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[k]
}

type fakeSource struct {
	clusters []model.ClusterRef
	fail     map[string]error
	block    chan struct{} // when set, Reconcile waits for it
	calls    chan string
}

func (f *fakeSource) Clusters() []model.ClusterRef { return f.clusters }

func (f *fakeSource) Reconcile(ctx context.Context, env string) error {
	f.calls <- env
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.fail[env]
}

type hookFunc func(context.Context, time.Time)

func (h hookFunc) Tick(ctx context.Context, now time.Time) { h(ctx, now) }

// newTestLoop drives the loop with a manual tick channel.
func newTestLoop(src Source, m metrics.MetricsManager, hooks ...Hook) (*Loop, chan time.Time) {
	ch := make(chan time.Time)
	l := New(src, m, time.Minute, time.Second, hooks...)
	l.newTicker = func(time.Duration) (<-chan time.Time, func()) { return ch, func() {} }
	return l, ch
}

func start(t *testing.T, l *Loop) (cancel func()) {
	t.Helper()
	ctx, c := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = l.Start(ctx); close(done) }()
	return func() {
		c()
		<-done
		sctx, sc := context.WithTimeout(context.Background(), 2*time.Second)
		defer sc()
		if err := l.Shutdown(sctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}
}

func TestLoop_TickReconcilesEveryClusterAndCountsResults(t *testing.T) {
	t.Parallel()

	src := &fakeSource{
		clusters: []model.ClusterRef{{Env: "stg", Name: "stg-c"}, {Env: "prd", Name: "prd-c"}},
		fail:     map[string]error{"prd": errors.New("boom")},
		calls:    make(chan string, 10),
	}
	m := &recMetrics{counts: map[string]int{}}
	hooked := make(chan time.Time, 1)
	l, ticks := newTestLoop(src, m, hookFunc(func(_ context.Context, now time.Time) { hooked <- now }))
	want := time.Unix(100, 0)
	l.now = func() time.Time { return want }
	cancel := start(t, l)

	ticks <- time.Now()
	if got := <-hooked; !got.Equal(want) {
		t.Errorf("hook now = %v, want %v", got, want)
	}
	cancel()

	if m.get("stg-c/success") != 1 || m.get("prd-c/error") != 1 {
		t.Errorf("counts = %v, want stg-c/success=1 prd-c/error=1", m.counts)
	}
}

func TestLoop_SkipsTickWhileRunning(t *testing.T) {
	t.Parallel()

	src := &fakeSource{
		clusters: []model.ClusterRef{{Env: "stg", Name: "stg-c"}},
		block:    make(chan struct{}),
		calls:    make(chan string, 10),
	}
	m := &recMetrics{counts: map[string]int{}}
	l, ticks := newTestLoop(src, m)
	cancel := start(t, l)

	ticks <- time.Now()
	<-src.calls // first run is in flight
	ticks <- time.Now()
	ticks <- time.Now() // accepted only after the previous tick was handled
	close(src.block)
	cancel()

	if n := len(src.calls); n != 0 {
		t.Errorf("extra reconcile calls = %d, want 0 (second tick skipped)", n)
	}
	if m.get("stg-c/success") != 1 {
		t.Errorf("counts = %v, want one success", m.counts)
	}
}

func TestLoop_ShutdownCancelsInflightReconcile(t *testing.T) {
	t.Parallel()

	src := &fakeSource{
		clusters: []model.ClusterRef{{Env: "stg", Name: "stg-c"}},
		block:    make(chan struct{}),
		calls:    make(chan string, 10),
	}
	m := &recMetrics{counts: map[string]int{}}
	l, ticks := newTestLoop(src, m)
	cancel := start(t, l)

	ticks <- time.Now()
	<-src.calls
	cancel()

	if m.get("stg-c/error") != 1 {
		t.Errorf("counts = %v, want the canceled run counted as error", m.counts)
	}
}

func TestLoop_DisabledReturnsImmediately(t *testing.T) {
	t.Parallel()

	l := New(&fakeSource{}, &recMetrics{counts: map[string]int{}}, 0, time.Second)
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}
