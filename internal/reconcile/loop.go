// Package reconcile runs the periodic background work of the controller.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

// Source is the part of model.WorkloadStore the loop needs.
type Source interface {
	Clusters() []model.ClusterRef
	Reconcile(ctx context.Context, env string) error
}

// Hook is per-tick work that runs after the clusters are reconciled (for example auto-stop).
type Hook interface {
	Tick(ctx context.Context, now time.Time)
}

// Loop implements server.Lifecycle: it reconciles every cluster each interval.
// A tick is skipped while the previous one is still running.
type Loop struct {
	src      Source
	metrics  metrics.MetricsManager
	interval time.Duration
	timeout  time.Duration
	hooks    []Hook

	newTicker func(time.Duration) (<-chan time.Time, func())
	now       func() time.Time

	mu      sync.Mutex
	running bool
	wg      sync.WaitGroup
}

// New builds a Loop. interval <= 0 disables it; timeout bounds each cluster reconcile.
func New(src Source, m metrics.MetricsManager, interval, timeout time.Duration, hooks ...Hook) *Loop {
	return &Loop{
		src: src, metrics: m, interval: interval, timeout: timeout, hooks: hooks,
		newTicker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		now: time.Now,
	}
}

// Name implements server.Lifecycle.
func (l *Loop) Name() string { return "reconciler" }

// Start runs the loop until ctx is canceled.
func (l *Loop) Start(ctx context.Context) error {
	if l.interval <= 0 {
		slog.Info("background reconcile disabled", slog.String("reason", "RECONCILE_INTERVAL is 0"))
		return nil
	}
	ticks, stop := l.newTicker(l.interval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			l.launch(ctx)
		}
	}
}

// Shutdown waits for the in-flight tick, or returns when ctx expires.
func (l *Loop) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for reconcile tick: %w", ctx.Err())
	}
}

func (l *Loop) launch(ctx context.Context) {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		slog.Debug("reconcile tick skipped", slog.String("reason", "previous tick still running"))
		return
	}
	l.running = true
	l.wg.Add(1)
	l.mu.Unlock()

	go func() {
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			l.running = false
			l.mu.Unlock()
		}()
		l.tick(ctx)
	}()
}

func (l *Loop) tick(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range l.src.Clusters() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, l.timeout)
			defer cancel()
			if err := l.src.Reconcile(cctx, c.Env); err != nil {
				l.metrics.IncReconcile(c.Name, "error")
				slog.Warn("background reconcile failed", slog.String("cluster.name", c.Name), slog.String("cluster.env", c.Env), slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()), slog.String("operation", "reconcile"))
				return
			}
			l.metrics.IncReconcile(c.Name, "success")
		}()
	}
	wg.Wait()

	now := l.now()
	for _, h := range l.hooks {
		h.Tick(ctx, now)
	}
}
