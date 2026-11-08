package frontend

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/rollout"
	"github.com/murasame29/workload-knocker/internal/runlimit"
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
	// ActionExtend moves the auto-stop of a running workload; it is authorized like a start.
	ActionExtend Action = "extend"
)

// Change is one plan line: Names (of Action) belonging to the entry shown as Label.
type Change struct {
	Action Action
	Label  string
	Names  []string
}

// Reason says why an operation was rejected.
type Reason string

const (
	// ReasonUnauthorized: the authorization policy does not allow the subject.
	ReasonUnauthorized Reason = "unauthorized"
	// ReasonChatStopDisabled: OPERATION_MODE=scheduled-stop leaves stopping to the nightly job.
	ReasonChatStopDisabled Reason = "chat-stop-disabled"
	// ReasonInvalidDuration: the requested run duration is out of range; see Rejection.Detail.
	ReasonInvalidDuration Reason = "invalid-duration"
)

// Rejection is a plan or apply line that was refused; Label is the display target.
type Rejection struct {
	Action Action
	Label  string
	Names  []string
	Reason Reason
	// Detail explains ReasonInvalidDuration.
	Detail string
}

// Message is the user-facing explanation, safe to show in a chat.
func (r Rejection) Message() string {
	switch r.Reason {
	case ReasonChatStopDisabled:
		return "stopping from chat is disabled (OPERATION_MODE=scheduled-stop); workloads are stopped by the nightly job"
	case ReasonInvalidDuration:
		return r.Detail
	}
	return "you are not authorized to " + string(r.Action) + " this"
}

// Plan is the diff between the desired and current state of one namespace.
type Plan struct {
	Cluster   config.Cluster
	Namespace string
	Changes   []Change
	// ToStart / ToStop are the workload names to change.
	ToStart []string
	ToStop  []string
	// Rejected are the changes refused for the subject; they are not in Changes / ToStart / ToStop.
	Rejected []Rejection
}

// Empty reports whether the plan has nothing to change.
func (p Plan) Empty() bool { return len(p.ToStart) == 0 && len(p.ToStop) == 0 }

// ApplyRequest is the confirmed set of changes for one namespace.
type ApplyRequest struct {
	Cluster   config.Cluster
	Namespace string
	// User is the display name recorded as the starter.
	User string
	// Subject is checked against the policy again at apply time; never trust the submitted lists.
	Subject authz.Subject
	// RequestID ties the audit events to the preview of this operation; generated when empty.
	RequestID string
	ToStart   []string
	ToStop    []string
	// Durations are the requested run durations by workload name; absent or 0 selects the default.
	Durations map[string]time.Duration
}

// Applied is the outcome of one workload operation.
type Applied struct {
	Action Action
	Name   string
	// Found is false when the store did not know the workload.
	Found bool
	// StopAt is the scheduled auto-stop of a start; zero when none.
	StopAt time.Time
}

// ApplyResult is the data a frontend renders after applying a plan.
type ApplyResult struct {
	Applied []Applied
	// Rejected are the requested operations refused at apply time.
	Rejected []Rejection
	// AutoStop is true when started workloads are stopped again by the nightly auto-stop.
	AutoStop bool
}

// Service is the frontend-independent workload operation logic.
type Service struct {
	store            model.WorkloadStore
	metrics          metrics.MetricsManager
	reconcileTimeout time.Duration
	authz            authz.Authorizer
	mode             mode.Mode
	audit            audit.Recorder
	rollout          *rollout.Tracker
	runDefaults      runlimit.Defaults
	now              func() time.Time
}

// Option customizes a Service.
type Option func(*Service)

// WithAuthorizer sets the policy. Without it every operation is denied (fail closed).
func WithAuthorizer(a authz.Authorizer) Option { return func(s *Service) { s.authz = a } }

// WithMode sets the operation mode; scheduled-stop forbids stopping from chat.
func WithMode(m mode.Mode) Option { return func(s *Service) { s.mode = m } }

// WithRecorder replaces the audit event recorder (default: the slog default logger).
func WithRecorder(r audit.Recorder) Option { return func(s *Service) { s.audit = r } }

// WithRollout enables per-workload rollout progress after Apply; without it Track reports false.
func WithRollout(t *rollout.Tracker) Option { return func(s *Service) { s.rollout = t } }

// WithRunDefaults sets the controller-wide run duration settings (zero value: no default, no cap).
func WithRunDefaults(d runlimit.Defaults) Option { return func(s *Service) { s.runDefaults = d } }

// WithClock replaces the clock used to compute stop times (default: time.Now).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService builds a Service. reconcileTimeout bounds each on-demand Reconcile.
func NewService(store model.WorkloadStore, m metrics.MetricsManager, reconcileTimeout time.Duration, opts ...Option) *Service {
	s := &Service{store: store, metrics: m, reconcileTimeout: reconcileTimeout, audit: audit.NewSlog(), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// check returns "" when sub may perform a on the workload, else the rejection reason.
func (s *Service) check(sub authz.Subject, env, namespace string, a Action, name string) Reason {
	if a == ActionStop && !s.mode.AllowChatStop() {
		return ReasonChatStopDisabled
	}
	if a == ActionExtend {
		a = ActionStart
	}
	if s.authz == nil || !s.authz.Allowed(sub, env, namespace, name, authz.Action(a)) {
		return ReasonUnauthorized
	}
	return ""
}

// operable reports whether sub may start or stop the workload at all.
func (s *Service) operable(sub authz.Subject, env, namespace, name string) bool {
	return s.check(sub, env, namespace, ActionStart, name) == "" || s.check(sub, env, namespace, ActionStop, name) == ""
}

// Reconcile refreshes the cluster state; on failure it logs and the cached state is used.
func (s *Service) Reconcile(env string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.reconcileTimeout)
	defer cancel()
	if err := s.store.Reconcile(ctx, env); err != nil {
		slog.Warn("ScaledObject reconcile failed, continuing with cache", slog.String("error.message", err.Error()))
	}
}

// Namespaces returns the namespaces of env that hold a workload sub may operate, sorted a-z.
// total is the namespace count before the policy filter.
func (s *Service) Namespaces(sub authz.Subject, env string) (allowed []string, total int) {
	all := slices.Clone(s.store.Namespaces(env))
	slices.Sort(all)
	for _, ns := range all {
		if slices.ContainsFunc(s.store.WorkloadsByNamespace(env, ns), func(w *model.Workload) bool {
			return s.operable(sub, env, ns, w.Name)
		}) {
			allowed = append(allowed, ns)
		}
	}
	return allowed, len(all)
}

// List groups the workloads of env/namespace that sub may operate into entries and applies limits.
func (s *Service) List(sub authz.Subject, env, namespace string, limits Limits) Listing {
	visible := slices.DeleteFunc(slices.Clone(s.store.WorkloadsByNamespace(env, namespace)), func(w *model.Workload) bool {
		return !s.operable(sub, env, namespace, w.Name)
	})
	entries := GroupEntries(visible)
	l := Listing{Entries: entries, Limits: limits}
	if limits.MaxEntries > 0 && len(entries) > limits.MaxEntries {
		l.Truncated = len(entries) - limits.MaxEntries
		l.Entries = entries[:limits.MaxEntries]
	}
	return l
}

// ChatStopAllowed reports whether chat may stop workloads in the current operation mode.
func (s *Service) ChatStopAllowed() bool { return s.mode.AllowChatStop() }

// Plan is BuildPlan for sub: changes the policy or mode refuses move to Plan.Rejected.
// requestID ties the audit events of the refusals to the later Apply.
func (s *Service) Plan(sub authz.Subject, cl config.Cluster, namespace string, entries []Entry, selected map[string]bool, requestID string) Plan {
	plan := buildPlan(cl, namespace, entries, selected, func(a Action, name string) Reason {
		return s.check(sub, cl.Env, namespace, a, name)
	})
	for _, r := range plan.Rejected {
		for _, name := range r.Names {
			s.record(sub, cl, namespace, name, r.Action, audit.Denied, requestID, nil)
		}
	}
	return plan
}

var errNotFound = errors.New("workload not found")

// record emits the audit event of one workload operation requested by sub.
func (s *Service) record(sub authz.Subject, cl config.Cluster, namespace, name string, a Action, outcome audit.Outcome, requestID string, err error, opts ...func(*audit.Record)) {
	source := audit.Source(sub.Platform)
	event := audit.WorkloadStart
	switch a {
	case ActionStop:
		event = audit.WorkloadStop
	case ActionExtend:
		event = audit.WorkloadExtend
	}
	rec := audit.Record{
		Event: event, Outcome: outcome, Source: source, RequestID: requestID,
		EndUser: audit.EndUser(source, sub.User),
		Cluster: cl.Name, Namespace: namespace, WorkloadName: name, Err: err,
	}
	for _, o := range opts {
		o(&rec)
	}
	for _, w := range s.store.WorkloadsByNamespace(cl.Env, namespace) {
		if w.Name == name {
			rec.WorkloadKind = w.Kind
			break
		}
	}
	s.audit.Record(context.Background(), rec)
}

// BuildPlan diffs the selection (checked = wants running) against the current state, without authorization.
// An entry counts as ON if any member name is in selected.
func BuildPlan(cl config.Cluster, namespace string, entries []Entry, selected map[string]bool) Plan {
	return buildPlan(cl, namespace, entries, selected, func(Action, string) Reason { return "" })
}

func buildPlan(cl config.Cluster, namespace string, entries []Entry, selected map[string]bool, decide func(Action, string) Reason) Plan {
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
		startNames = plan.reject(e, ActionStart, startNames, decide)
		stopNames = plan.reject(e, ActionStop, stopNames, decide)
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

// reject moves the names decide refuses into p.Rejected and returns the rest.
func (p *Plan) reject(e Entry, a Action, names []string, decide func(Action, string) Reason) []string {
	var kept, refused []string
	var reason Reason
	for _, n := range names {
		if r := decide(a, n); r != "" {
			refused, reason = append(refused, n), r
			continue
		}
		kept = append(kept, n)
	}
	if len(refused) > 0 {
		p.Rejected = append(p.Rejected, Rejection{Action: a, Label: e.ChangeLabel(refused), Names: refused, Reason: reason})
	}
	return kept
}

// Apply runs the requested starts then stops through the store, recording metrics.
// Every entry is checked against the policy and mode again; refused ones land in ApplyResult.Rejected.
func (s *Service) Apply(req ApplyRequest) ApplyResult {
	cl := req.Cluster
	var res ApplyResult
	started := 0
	if req.RequestID == "" {
		req.RequestID = audit.NewRequestID()
	}

	for _, name := range req.ToStart {
		if name == "" {
			continue
		}
		if s.rejectApply(&res, req, ActionStart, name) {
			continue
		}
		sch := s.schedule(cl.Env, req.Namespace, name, req.Durations[name])
		if sch.Err != nil {
			res.Rejected = append(res.Rejected, Rejection{Action: ActionStart, Label: name, Names: []string{name}, Reason: ReasonInvalidDuration, Detail: name + ": " + sch.Err.Error()})
			s.record(req.Subject, cl, req.Namespace, name, ActionStart, audit.Failure, req.RequestID, sch.Err)
			continue
		}
		begin := time.Now()
		ok := s.store.Start(cl.Env, req.Namespace, name, req.User, sch.StopAt)
		s.metrics.ObserveOperation(cl.Env, cl.Name, string(ActionStart), resultLabel(ok), time.Since(begin))
		res.Applied = append(res.Applied, Applied{Action: ActionStart, Name: name, Found: ok, StopAt: sch.StopAt})
		started++
		s.recordApplied(req, ActionStart, name, ok, withSchedule(sch))
	}

	for _, name := range req.ToStop {
		if name == "" {
			continue
		}
		if s.rejectApply(&res, req, ActionStop, name) {
			continue
		}
		begin := time.Now()
		ok := s.store.Stop(cl.Env, req.Namespace, name)
		s.metrics.ObserveOperation(cl.Env, cl.Name, string(ActionStop), resultLabel(ok), time.Since(begin))
		res.Applied = append(res.Applied, Applied{Action: ActionStop, Name: name, Found: ok})
		s.recordApplied(req, ActionStop, name, ok)
	}

	res.AutoStop = started > 0
	return res
}

// rejectApply records a rejection and returns true when the operation must not run.
func (s *Service) rejectApply(res *ApplyResult, req ApplyRequest, a Action, name string) bool {
	reason := s.check(req.Subject, req.Cluster.Env, req.Namespace, a, name)
	if reason == "" {
		return false
	}
	res.Rejected = append(res.Rejected, Rejection{Action: a, Label: name, Names: []string{name}, Reason: reason})
	s.record(req.Subject, req.Cluster, req.Namespace, name, a, audit.Denied, req.RequestID, nil)
	return true
}

func (s *Service) recordApplied(req ApplyRequest, a Action, name string, found bool, opts ...func(*audit.Record)) {
	if found {
		s.record(req.Subject, req.Cluster, req.Namespace, name, a, audit.Success, req.RequestID, nil, opts...)
		return
	}
	s.record(req.Subject, req.Cluster, req.Namespace, name, a, audit.Failure, req.RequestID, errNotFound, opts...)
}

func withSchedule(sch Schedule) func(*audit.Record) {
	return func(r *audit.Record) { r.Duration, r.StopAt = sch.Duration, sch.StopAt }
}

// resultLabel maps a store outcome to the metrics result label.
func resultLabel(ok bool) string {
	if ok {
		return "success"
	}
	return "not_found"
}

// RolloutEnabled reports whether rollout progress is tracked after Apply.
func (s *Service) RolloutEnabled() bool { return s.rollout != nil }

// RolloutTimeout is the watch timeout of the rollout tracker.
func (s *Service) RolloutTimeout() time.Duration {
	if s.rollout == nil {
		return 0
	}
	return s.rollout.Timeout()
}

// RolloutTargets lists the applied workloads that exist and are worth following.
func (s *Service) RolloutTargets(req ApplyRequest, res ApplyResult) []rollout.Target {
	kinds := map[string]string{}
	for _, w := range s.store.WorkloadsByNamespace(req.Cluster.Env, req.Namespace) {
		kinds[w.Name] = w.Kind
	}
	var targets []rollout.Target
	for _, a := range res.Applied {
		if !a.Found {
			continue
		}
		dir := rollout.Start
		if a.Action == ActionStop {
			dir = rollout.Stop
		}
		targets = append(targets, rollout.Target{Env: req.Cluster.Env, Namespace: req.Namespace, Name: a.Name, Kind: kinds[a.Name], Direction: dir})
	}
	return targets
}

// Track follows targets until all are final or ctx ends. onChange gets the initial snapshot,
// then one snapshot per poll in which something changed. No-op without WithRollout.
func (s *Service) Track(ctx context.Context, targets []rollout.Target, onChange func(rollout.Snapshot)) {
	if s.rollout == nil || len(targets) == 0 {
		return
	}
	s.rollout.Watch(ctx, targets, onChange)
}
