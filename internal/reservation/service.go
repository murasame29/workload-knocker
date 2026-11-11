package reservation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/notify"
	"github.com/murasame29/workload-knocker/internal/rollout"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// Limits of a reservation.
const (
	// MaxAhead bounds how far in the future a reservation may be.
	MaxAhead = 30 * 24 * time.Hour
	// MaxWorkloads bounds the workloads of one reservation.
	MaxWorkloads = 25
	// maxPending bounds the pending reservations of one env.
	maxPending = 100
	// retention keeps finished reservations visible in the list for a while.
	retention = 24 * time.Hour
	// callTimeout bounds one load or save of the reservations of an env.
	callTimeout = 10 * time.Second
)

// Store is the part of the workload store the service needs.
type Store interface {
	Clusters() []model.ClusterRef
	WorkloadsByNamespace(env, ns string) []*model.Workload
	Start(env, ns, name, user string, stopAt time.Time) error
	ReservationStore(env string) (k8s.KeyStore, bool)
}

// Notifier posts reservation messages to the notify channel.
type Notifier interface {
	Started(ctx context.Context, e notify.Event)
	Announce(ctx context.Context, env, text string)
}

// Watcher follows started workloads so their ready message is posted (rollout.Tracker).
type Watcher interface {
	Watch(ctx context.Context, targets []rollout.Target, onChange func(rollout.Snapshot))
}

// Metrics records the reservation metrics.
type Metrics interface {
	SetReservations(env, state string, n int)
	IncReservationExecution(env, result string)
}

// Settings are the controller-wide reservation settings.
type Settings struct {
	// Warmup is how long before the requested time workloads are started (annotation overrides).
	Warmup time.Duration
	// Grace is how long after the start time a reservation still runs (controller was down).
	Grace time.Duration
	// Reminder is how long before the start the reminder is posted; 0 disables it.
	Reminder time.Duration
	// Run are the run duration limits applied to the requested duration.
	Run runlimit.Defaults
	// Location returns the time zone of env for notify messages; nil means UTC.
	Location func(env string) *time.Location
}

func (st Settings) location(env string) *time.Location {
	if st.Location != nil {
		if loc := st.Location(env); loc != nil {
			return loc
		}
	}
	return time.UTC
}

// Service creates, lists, cancels and executes reservations.
type Service struct {
	store    Store
	authz    authz.Authorizer
	settings Settings
	notifier Notifier
	watcher  Watcher
	metrics  Metrics
	audit    audit.Recorder
	now      func() time.Time
	// watchCtx bounds the rollout watches started by executions.
	watchCtx context.Context

	mu    sync.Mutex
	books map[string][]Reservation
}

// Option customizes a Service.
type Option func(*Service)

// WithNotifier sets the notify channel poster.
func WithNotifier(n Notifier) Option { return func(s *Service) { s.notifier = n } }

// WithWatcher sets the rollout watcher used for ready messages.
func WithWatcher(w Watcher) Option { return func(s *Service) { s.watcher = w } }

// WithMetrics sets the metrics sink.
func WithMetrics(m Metrics) Option { return func(s *Service) { s.metrics = m } }

// WithRecorder replaces the audit recorder (default: slog).
func WithRecorder(r audit.Recorder) Option { return func(s *Service) { s.audit = r } }

// WithClock replaces the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithWatchContext bounds the rollout watches of executions (default: background).
func WithWatchContext(ctx context.Context) Option { return func(s *Service) { s.watchCtx = ctx } }

// New builds a Service.
func New(st Store, az authz.Authorizer, settings Settings, opts ...Option) *Service {
	s := &Service{store: st, authz: az, settings: settings, audit: audit.NewSlog(), now: time.Now, watchCtx: context.Background(), books: map[string][]Reservation{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// CreateRequest is a reservation to create.
type CreateRequest struct {
	Env       string
	Namespace string
	Workloads []string
	At        time.Time
	// Duration is the requested run length; 0 uses each workload's default.
	Duration time.Duration
	Subject  authz.Subject
	Source   audit.Source
}

// Create validates and stores a reservation. Errors wrap ErrInvalid, ErrNotFound or ErrForbidden.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Reservation, error) {
	now := s.now()
	names := slices.Compact(slices.Sorted(slices.Values(req.Workloads)))
	switch {
	case len(names) == 0 || names[0] == "":
		return Reservation{}, fmt.Errorf("%w: no workloads", ErrInvalid)
	case len(names) > MaxWorkloads:
		return Reservation{}, fmt.Errorf("%w: at most %d workloads", ErrInvalid, MaxWorkloads)
	case !req.At.After(now):
		return Reservation{}, fmt.Errorf("%w: the time must be in the future", ErrInvalid)
	case req.At.Sub(now) > MaxAhead:
		return Reservation{}, fmt.Errorf("%w: at most %d days ahead", ErrInvalid, int(MaxAhead.Hours()/24))
	}
	if _, ok := s.store.ReservationStore(req.Env); !ok {
		return Reservation{}, fmt.Errorf("cluster env %q: %w", req.Env, ErrNotFound)
	}
	known := s.workloads(req.Env, req.Namespace)
	for _, name := range names {
		if !s.authz.Allowed(req.Subject, req.Env, req.Namespace, name, authz.ActionStart) {
			return Reservation{}, fmt.Errorf("%s: %w to start it", name, ErrForbidden)
		}
		w, ok := known[name]
		if !ok {
			return Reservation{}, fmt.Errorf("workload %s/%s: %w", req.Namespace, name, ErrNotFound)
		}
		if _, err := s.settings.Run.For(w).Resolve(req.Duration); err != nil {
			return Reservation{}, fmt.Errorf("%w: %s: %v", ErrInvalid, name, err)
		}
	}
	id, err := newID()
	if err != nil {
		return Reservation{}, fmt.Errorf("generate reservation id: %w", err)
	}
	r := Reservation{
		ID: id, Namespace: req.Namespace, Workloads: names, At: req.At.UTC(), Duration: req.Duration,
		Creator: CreatorOf(req.Subject), CreatedAt: now.UTC(), State: Pending,
	}
	err = s.update(ctx, req.Env, func(list []Reservation) ([]Reservation, error) {
		pending := 0
		for _, o := range list {
			if o.State == Pending {
				pending++
			}
		}
		if pending >= maxPending {
			return nil, fmt.Errorf("%w: at most %d pending reservations per cluster", ErrInvalid, maxPending)
		}
		return append(list, r), nil
	})
	outcome := audit.Success
	if err != nil {
		outcome = audit.Failure
	}
	s.record(ctx, req.Env, r, audit.ReservationCreate, req.Source, outcome, "", err)
	if err != nil {
		return Reservation{}, err
	}
	return r, nil
}

// List returns the reservations of env sorted by time, finished ones kept for a day.
func (s *Service) List(ctx context.Context, env string) ([]Reservation, error) {
	list, err := s.book(ctx, env)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b Reservation) int { return a.At.Compare(b.At) })
	return list, nil
}

// Visible returns the pending reservations of env that sub created or may cancel.
func (s *Service) Visible(ctx context.Context, sub authz.Subject, env string) []Reservation {
	list, err := s.List(ctx, env)
	if err != nil {
		slog.Warn("failed to load reservations", slog.String("cluster.env", env), slog.String("error.message", err.Error()))
		return nil
	}
	var out []Reservation
	for _, r := range list {
		if r.State == Pending && s.mayCancel(sub, env, r) {
			out = append(out, r)
		}
	}
	return out
}

// mayCancel: the creator, or a subject allowed to stop every workload of the reservation.
func (s *Service) mayCancel(sub authz.Subject, env string, r Reservation) bool {
	if r.Creator.Is(sub) {
		return true
	}
	for _, name := range r.Workloads {
		if !s.authz.Allowed(sub, env, r.Namespace, name, authz.ActionStop) {
			return false
		}
	}
	return true
}

// Cancel cancels a pending reservation. Errors wrap ErrNotFound or ErrForbidden.
func (s *Service) Cancel(ctx context.Context, env, id string, sub authz.Subject, source audit.Source) (Reservation, error) {
	var got Reservation
	err := s.update(ctx, env, func(list []Reservation) ([]Reservation, error) {
		for i := range list {
			if list[i].ID != id || list[i].State != Pending {
				continue
			}
			if !s.mayCancel(sub, env, list[i]) {
				return nil, fmt.Errorf("%w to cancel this reservation", ErrForbidden)
			}
			list[i].State, list[i].DoneAt = Cancelled, s.now().UTC()
			got = list[i]
			return list, nil
		}
		return nil, fmt.Errorf("pending reservation %q: %w", id, ErrNotFound)
	})
	if err != nil {
		return Reservation{}, err
	}
	s.record(ctx, env, got, audit.ReservationCancel, source, audit.Success, sub.User, nil)
	return got, nil
}

// Tick implements reconcile.Hook: it reminds, executes, marks missed and prunes reservations.
func (s *Service) Tick(ctx context.Context, now time.Time) {
	for _, c := range s.store.Clusters() {
		if _, ok := s.store.ReservationStore(c.Env); !ok {
			continue
		}
		var runs []Reservation
		var notes []string
		err := s.update(ctx, c.Env, func(list []Reservation) ([]Reservation, error) {
			kept := list[:0]
			for _, r := range list {
				if r.State != Pending {
					if now.Sub(r.DoneAt) < retention {
						kept = append(kept, r)
					}
					continue
				}
				start := r.At.Add(-s.warmup(c.Env, r))
				switch {
				case now.Before(start):
					if s.settings.Reminder > 0 && !r.Reminded && !now.Before(start.Add(-s.settings.Reminder)) {
						r.Reminded = true
						notes = append(notes, reminderText(c.Env, r, start, s.settings.location(c.Env)))
					}
				case now.Sub(start) > s.settings.Grace:
					r.State, r.DoneAt = Missed, now.UTC()
					r.Note = fmt.Sprintf("not started: the controller was unavailable until %s", now.UTC().Format(time.RFC3339))
					notes = append(notes, missedText(c.Env, r, s.settings.location(c.Env)))
					s.count(c.Env, "missed")
				default:
					runs = append(runs, r)
					r.State, r.DoneAt = Started, now.UTC()
				}
				kept = append(kept, r)
			}
			return kept, nil
		})
		if err != nil {
			slog.Warn("reservation tick failed", slog.String("cluster.env", c.Env), slog.String("error.message", err.Error()))
			continue
		}
		for _, text := range notes {
			s.announce(ctx, c.Env, text)
		}
		for _, r := range runs {
			s.execute(ctx, c, r)
		}
		s.publish(c.Env)
	}
}

// execute starts the workloads of r. The state was set to Started before, so a crash in between
// cannot start them twice; a reservation where nothing started is rewritten to Failed.
func (s *Service) execute(ctx context.Context, c model.ClusterRef, r Reservation) {
	sub := r.Creator.Subject()
	known := s.workloads(c.Env, r.Namespace)
	var started []notify.Workload
	var targets []rollout.Target
	var problems []string
	for _, name := range r.Workloads {
		w, ok := known[name]
		var err error
		switch {
		case !ok:
			err = errors.New("workload not found")
		case !s.authz.Allowed(sub, c.Env, r.Namespace, name, authz.ActionStart):
			err = ErrForbidden
		}
		var stopAt time.Time
		if err == nil {
			var d time.Duration
			if d, err = s.settings.Run.For(w).Resolve(r.Duration); err == nil {
				if d > 0 {
					stopAt = r.At.Add(d)
				}
				err = s.store.Start(c.Env, r.Namespace, name, r.Creator.User, stopAt)
			}
		}
		outcome := audit.Success
		if err != nil {
			outcome = audit.Failure
			problems = append(problems, name+": "+err.Error())
		} else {
			started = append(started, notify.Workload{Namespace: r.Namespace, Name: name, StopAt: stopAt, ReadyMessage: w.ReadyMessage})
			targets = append(targets, rollout.Target{Env: c.Env, Namespace: r.Namespace, Name: name, Kind: w.Kind, Direction: rollout.Start})
		}
		s.audit.Record(ctx, audit.Record{
			Event: audit.ReservationExecute, Outcome: outcome, Source: audit.SourceScheduler, RequestID: r.ID,
			EndUser: audit.EndUser(audit.Source(r.Creator.Platform), r.Creator.User),
			Cluster: c.Name, Namespace: r.Namespace, WorkloadName: name, WorkloadKind: kindOf(w), StopAt: stopAt, Err: err,
		})
	}

	result := "success"
	switch {
	case len(started) == 0:
		result = "failed"
	case len(problems) > 0:
		result = "partial"
	}
	s.count(c.Env, result)
	if len(problems) > 0 {
		note := strings.Join(problems, "; ")
		err := s.update(ctx, c.Env, func(list []Reservation) ([]Reservation, error) {
			for i := range list {
				if list[i].ID == r.ID {
					list[i].Note = note
					if len(started) == 0 {
						list[i].State = Failed
					}
				}
			}
			return list, nil
		})
		if err != nil {
			slog.Warn("failed to record reservation result", slog.String("cluster.env", c.Env), slog.String("reservation.id", r.ID), slog.String("error.message", err.Error()))
		}
		s.announce(ctx, c.Env, fmt.Sprintf("⚠ Reservation %s (%s / %s) could not start %s", r.ID, strings.ToUpper(c.Env), r.Namespace, note))
	}
	if len(started) == 0 {
		return
	}
	if s.notifier != nil {
		s.notifier.Started(ctx, notify.Event{Kind: notify.KindStart, Env: c.Env, ClusterName: c.Name, Namespace: r.Namespace, Actor: "📅 reservation by " + r.Creator.Mention(), StartedBy: r.Creator.User, Workloads: started})
	}
	if s.watcher != nil {
		go s.watcher.Watch(s.watchCtx, targets, func(rollout.Snapshot) {})
	}
}

func kindOf(w *model.Workload) string {
	if w == nil {
		return ""
	}
	return w.Kind
}

// warmup is the largest warmup annotation of r's workloads, or the default.
func (s *Service) warmup(env string, r Reservation) time.Duration {
	d := time.Duration(0)
	known := s.workloads(env, r.Namespace)
	for _, name := range r.Workloads {
		if w, ok := known[name]; ok && w.Warmup > d {
			d = w.Warmup
		}
	}
	if d == 0 {
		return s.settings.Warmup
	}
	return d
}

// Warmup returns the warmup that applies to the workloads of namespace in env.
func (s *Service) Warmup(env, namespace string, names []string) time.Duration {
	return s.warmup(env, Reservation{Namespace: namespace, Workloads: names})
}

func (s *Service) workloads(env, namespace string) map[string]*model.Workload {
	out := map[string]*model.Workload{}
	for _, w := range s.store.WorkloadsByNamespace(env, namespace) {
		out[w.Name] = w
	}
	return out
}

// book returns a copy of the reservations of env, loading them on first use.
func (s *Service) book(ctx context.Context, env string) ([]Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadLocked(ctx, env)
	return slices.Clone(list), err
}

func (s *Service) loadLocked(ctx context.Context, env string) ([]Reservation, error) {
	if list, ok := s.books[env]; ok {
		return list, nil
	}
	ks, ok := s.store.ReservationStore(env)
	if !ok {
		return nil, fmt.Errorf("cluster env %q: %w", env, ErrNotFound)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	raw, err := ks.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load reservations: %w", err)
	}
	var list []Reservation
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, fmt.Errorf("unmarshal reservations: %w", err)
		}
	}
	s.books[env] = list
	return list, nil
}

// update applies fn to the reservations of env and persists the result; a failed save keeps
// the previous list so memory and the ConfigMap do not diverge.
func (s *Service) update(ctx context.Context, env string, fn func([]Reservation) ([]Reservation, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadLocked(ctx, env)
	if err != nil {
		return err
	}
	next, err := fn(slices.Clone(list))
	if err != nil {
		return err
	}
	if next == nil {
		next = []Reservation{}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("marshal reservations: %w", err)
	}
	// Tick runs every interval; skip the cluster API call when nothing changed.
	if prev, err := json.Marshal(list); err == nil && (string(prev) == string(data) || (len(list) == 0 && len(next) == 0)) {
		return nil
	}
	ks, ok := s.store.ReservationStore(env)
	if !ok {
		return fmt.Errorf("cluster env %q: %w", env, ErrNotFound)
	}
	sctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := ks.Save(sctx, string(data)); err != nil {
		return fmt.Errorf("save reservations: %w", err)
	}
	s.books[env] = next
	return nil
}

func (s *Service) announce(ctx context.Context, env, text string) {
	if s.notifier != nil {
		s.notifier.Announce(ctx, env, text)
	}
}

func (s *Service) count(env, result string) {
	if s.metrics != nil {
		s.metrics.IncReservationExecution(env, result)
	}
}

// publish sets the reservation gauge of env per state.
func (s *Service) publish(env string) {
	if s.metrics == nil {
		return
	}
	s.mu.Lock()
	list := s.books[env]
	counts := map[State]int{}
	for _, r := range list {
		counts[r.State]++
	}
	s.mu.Unlock()
	for _, st := range States {
		s.metrics.SetReservations(env, string(st), counts[st])
	}
}

func (s *Service) record(ctx context.Context, env string, r Reservation, ev audit.Event, source audit.Source, outcome audit.Outcome, user string, err error) {
	if user == "" {
		user = r.Creator.User
	}
	cluster := env
	for _, c := range s.store.Clusters() {
		if c.Env == env {
			cluster = c.Name
		}
	}
	s.audit.Record(ctx, audit.Record{
		Event: ev, Outcome: outcome, Source: source, RequestID: r.ID, EndUser: audit.EndUser(source, user),
		Cluster: cluster, Namespace: r.Namespace, WorkloadName: strings.Join(r.Workloads, ","), StopAt: r.At, Err: err,
	})
	if err == nil {
		s.publish(env)
	}
}

func reminderText(env string, r Reservation, start time.Time, loc *time.Location) string {
	return fmt.Sprintf("📅 %s: %s start at %s to be ready by %s (%s / %s)", r.Creator.Mention(), strings.Join(r.Workloads, ", "),
		start.In(loc).Format("15:04 MST"), r.At.In(loc).Format("15:04 MST"), strings.ToUpper(env), r.Namespace)
}

func missedText(env string, r Reservation, loc *time.Location) string {
	return fmt.Sprintf("⚠ %s: the reservation of %s for %s was missed (%s / %s)", r.Creator.Mention(), strings.Join(r.Workloads, ", "),
		r.At.In(loc).Format("2006-01-02 15:04 MST"), strings.ToUpper(env), r.Namespace)
}
