package frontend

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

// Limits are the per-frontend display constraints. Zero means unlimited.
type Limits struct {
	// PageSize is the max entries per page (Slack: options per checkbox block).
	PageSize int
	// MaxEntries is the max entries shown in total; the rest are reported as truncated.
	MaxEntries int
}

// Listing is the entries of one cluster namespace after applying Limits.
type Listing struct {
	Entries []Entry
	// Truncated is the number of entries cut off by Limits.MaxEntries.
	Truncated int
	Limits    Limits
}

// Total returns the entry count before truncation.
func (l Listing) Total() int { return len(l.Entries) + l.Truncated }

// Pages splits Entries into pages of Limits.PageSize (one page when unlimited).
func (l Listing) Pages() [][]Entry {
	if len(l.Entries) == 0 {
		return nil
	}
	size := l.Limits.PageSize
	if size <= 0 {
		return [][]Entry{l.Entries}
	}
	var pages [][]Entry
	for start := 0; start < len(l.Entries); start += size {
		pages = append(pages, l.Entries[start:min(start+size, len(l.Entries))])
	}
	return pages
}

// Action is a start or stop operation.
type Action string

const (
	ActionStart Action = "start"
	ActionStop  Action = "stop"
)

// Change is one plan line: Names (of Action) belonging to the entry shown as Label.
type Change struct {
	Action Action
	Label  string
	Names  []string
}

// Plan is the diff between the desired and current state of one namespace.
type Plan struct {
	Cluster   config.Cluster
	Namespace string
	Changes   []Change
	// ToStart / ToStop are the workload names to change.
	ToStart []string
	ToStop  []string
}

// Empty reports whether the plan has nothing to change.
func (p Plan) Empty() bool { return len(p.ToStart) == 0 && len(p.ToStop) == 0 }

// ApplyRequest is the confirmed set of changes for one namespace.
type ApplyRequest struct {
	Cluster   config.Cluster
	Namespace string
	User      string
	ToStart   []string
	ToStop    []string
}

// Applied is the outcome of one workload operation.
type Applied struct {
	Action Action
	Name   string
	// Found is false when the store did not know the workload.
	Found bool
}

// ApplyResult is the data a frontend renders after applying a plan.
type ApplyResult struct {
	Applied []Applied
	// AutoStop is true when started workloads are stopped again by the nightly auto-stop.
	AutoStop bool
}

// Service is the frontend-independent workload operation logic.
type Service struct {
	store            model.WorkloadStore
	metrics          metrics.MetricsManager
	reconcileTimeout time.Duration
}

// NewService builds a Service. reconcileTimeout bounds each on-demand Reconcile.
func NewService(store model.WorkloadStore, m metrics.MetricsManager, reconcileTimeout time.Duration) *Service {
	return &Service{store: store, metrics: m, reconcileTimeout: reconcileTimeout}
}

// Reconcile refreshes the cluster state; on failure it logs and the cached state is used.
func (s *Service) Reconcile(env string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.reconcileTimeout)
	defer cancel()
	if err := s.store.Reconcile(ctx, env); err != nil {
		slog.Warn("ScaledObject reconcile failed, continuing with cache", slog.String("error.message", err.Error()))
	}
}

// Namespaces returns the managed namespaces of env sorted a-z.
func (s *Service) Namespaces(env string) []string {
	ns := slices.Clone(s.store.Namespaces(env))
	slices.Sort(ns)
	return ns
}

// List groups the workloads of env/namespace into entries and applies limits.
func (s *Service) List(env, namespace string, limits Limits) Listing {
	entries := GroupEntries(s.store.WorkloadsByNamespace(env, namespace))
	l := Listing{Entries: entries, Limits: limits}
	if limits.MaxEntries > 0 && len(entries) > limits.MaxEntries {
		l.Truncated = len(entries) - limits.MaxEntries
		l.Entries = entries[:limits.MaxEntries]
	}
	return l
}

// BuildPlan diffs the selection (checked = wants running) against the current state.
// An entry counts as ON if any member name is in selected.
func BuildPlan(cl config.Cluster, namespace string, entries []Entry, selected map[string]bool) Plan {
	plan := Plan{Cluster: cl, Namespace: namespace}
	for _, e := range entries {
		want := false
		for _, name := range e.Names() {
			if selected[name] {
				want = true
				break
			}
		}

		var startNames, stopNames []string
		for _, w := range e.Members {
			running := w.Replicas > 0
			switch {
			case want && !running:
				startNames = append(startNames, w.Name)
			case !want && running:
				stopNames = append(stopNames, w.Name)
			}
		}
		plan.ToStart = append(plan.ToStart, startNames...)
		plan.ToStop = append(plan.ToStop, stopNames...)
		if len(startNames) > 0 {
			plan.Changes = append(plan.Changes, Change{Action: ActionStart, Label: e.ChangeLabel(startNames), Names: startNames})
		}
		if len(stopNames) > 0 {
			plan.Changes = append(plan.Changes, Change{Action: ActionStop, Label: e.ChangeLabel(stopNames), Names: stopNames})
		}
	}
	return plan
}

// Apply runs the requested starts then stops through the store, recording metrics.
func (s *Service) Apply(req ApplyRequest) ApplyResult {
	cl := req.Cluster
	var res ApplyResult

	for _, name := range req.ToStart {
		if name == "" {
			continue
		}
		begin := time.Now()
		ok := s.store.Start(cl.Env, req.Namespace, name, req.User)
		s.metrics.ObserveOperation(cl.Env, cl.Name, string(ActionStart), resultLabel(ok), time.Since(begin))
		res.Applied = append(res.Applied, Applied{Action: ActionStart, Name: name, Found: ok})
		slog.Info("workload started", slog.String("service", name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", req.Namespace), slog.String("user.name", req.User))
	}

	for _, name := range req.ToStop {
		if name == "" {
			continue
		}
		begin := time.Now()
		ok := s.store.Stop(cl.Env, req.Namespace, name)
		s.metrics.ObserveOperation(cl.Env, cl.Name, string(ActionStop), resultLabel(ok), time.Since(begin))
		res.Applied = append(res.Applied, Applied{Action: ActionStop, Name: name, Found: ok})
		slog.Info("workload stopped", slog.String("service", name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", req.Namespace), slog.String("user.name", req.User))
	}

	res.AutoStop = len(req.ToStart) > 0
	return res
}

// resultLabel maps a store outcome to the metrics result label.
func resultLabel(ok bool) string {
	if ok {
		return "success"
	}
	return "not_found"
}
