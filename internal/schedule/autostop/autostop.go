// Package autostop stops workloads whose run duration ended and sends the stop reminder.
// It is a reconcile.Hook; the schedule is read from the persisted workload state, so it
// survives a restart.
package autostop

import (
	"context"
	"sync"
	"time"

	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

// Store is the part of model.WorkloadStore the hook needs.
type Store interface {
	Clusters() []model.ClusterRef
	Namespaces(env string) []string
	WorkloadsByNamespace(env, ns string) []*model.Workload
	Stop(env, ns, name string) error
}

// StopReminder is called once per workload and stop time shortly before the auto-stop.
// Delivery (for example to a notify channel with an Extend button) is wired by the caller.
type StopReminder interface {
	StopReminder(ctx context.Context, workload model.Workload, stopAt time.Time)
}

type noopReminder struct{}

func (noopReminder) StopReminder(context.Context, model.Workload, time.Time) {}

// Hook implements reconcile.Hook.
type Hook struct {
	store    Store
	metrics  metrics.MetricsManager
	audit    audit.Recorder
	reminder StopReminder
	// lead is how long before the stop the reminder fires; 0 disables it.
	lead time.Duration

	mu       sync.Mutex
	reminded map[remindKey]struct{}
	// refused holds the auto-stops the actuator refused; they are retried every tick but recorded once.
	refused map[remindKey]struct{}
}

type remindKey struct {
	env, namespace, name string
	stopAt               int64
}

// Option customizes a Hook.
type Option func(*Hook)

// WithReminder sets the reminder receiver (default: none).
func WithReminder(r StopReminder) Option {
	return func(h *Hook) {
		if r != nil {
			h.reminder = r
		}
	}
}

// WithRecorder replaces the audit event recorder (default: the slog default logger).
func WithRecorder(r audit.Recorder) Option { return func(h *Hook) { h.audit = r } }

// New builds the hook; lead is how long before the stop the reminder fires (0 disables it).
func New(s Store, m metrics.MetricsManager, lead time.Duration, opts ...Option) *Hook {
	h := &Hook{store: s, metrics: m, lead: lead, audit: audit.NewSlog(), reminder: noopReminder{}, reminded: map[remindKey]struct{}{}, refused: map[remindKey]struct{}{}}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Tick stops the workloads whose stop time passed and reminds those about to stop.
func (h *Hook) Tick(ctx context.Context, now time.Time) {
	live := map[remindKey]struct{}{}
	due := map[remindKey]struct{}{}
	defer h.forgetRefused(due)
	for _, c := range h.store.Clusters() {
		for _, ns := range h.store.Namespaces(c.Env) {
			for _, w := range h.store.WorkloadsByNamespace(c.Env, ns) {
				if w.Replicas == 0 || w.StopAt.IsZero() {
					continue
				}
				w.Env = c.Env
				k := remindKey{c.Env, w.Namespace, w.Name, w.StopAt.Unix()}
				if !now.Before(w.StopAt) {
					due[k] = struct{}{}
					h.stop(ctx, c, w, k)
					continue
				}
				live[k] = struct{}{}
				h.remind(ctx, k, w, now)
			}
		}
	}
	h.forget(live)
}

func (h *Hook) stop(ctx context.Context, c model.ClusterRef, w *model.Workload, k remindKey) {
	err := h.store.Stop(c.Env, w.Namespace, w.Name)
	if _, refused := model.AsRefused(err); refused {
		h.mu.Lock()
		_, seen := h.refused[k]
		h.refused[k] = struct{}{}
		h.mu.Unlock()
		if seen {
			return
		}
	}
	rec := audit.Record{
		Event: audit.WorkloadAutoStop, Outcome: audit.Success, Source: audit.SourceScheduler,
		RequestID: audit.NewRequestID(), EndUser: audit.EndUser(audit.SourceScheduler, ""),
		Cluster: c.Name, Namespace: w.Namespace, WorkloadName: w.Name, WorkloadKind: w.Kind,
		StopAt: w.StopAt,
	}
	result := "success"
	if err != nil {
		rec.Outcome, rec.Err = audit.Failure, err
		result = "error"
	}
	h.metrics.IncAutoStop(c.Env, c.Name, result)
	h.audit.Record(ctx, rec)
}

func (h *Hook) remind(ctx context.Context, k remindKey, w *model.Workload, now time.Time) {
	if h.lead <= 0 || now.Add(h.lead).Before(w.StopAt) {
		return
	}
	h.mu.Lock()
	_, done := h.reminded[k]
	h.reminded[k] = struct{}{}
	h.mu.Unlock()
	if !done {
		h.reminder.StopReminder(ctx, *w, w.StopAt)
	}
}

// forget drops reminders of workloads that stopped or whose stop time moved.
func (h *Hook) forget(live map[remindKey]struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k := range h.reminded {
		if _, ok := live[k]; !ok {
			delete(h.reminded, k)
		}
	}
}

// forgetRefused drops refusals that are no longer due (stopped, extended or removed).
func (h *Hook) forgetRefused(due map[remindKey]struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k := range h.refused {
		if _, ok := due[k]; !ok {
			delete(h.refused, k)
		}
	}
}
