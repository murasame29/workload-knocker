// Package notify posts public operation messages (start requested, ready, stopped, stop reminders)
// to the optional per-cluster notify channel of each chat platform. It is platform neutral:
// the chat adapters register a Poster. Without a configured channel nothing is posted.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/model"
)

// Kind is the operation an Event reports.
type Kind string

const (
	KindStart   Kind = "start"
	KindStop    Kind = "stop"
	KindStopAll Kind = "stop_all"
)

// Workload is one workload of an Event.
type Workload struct {
	Namespace string
	Name      string
	// ReadyMessage is the raw ready-message template of the workload; "" renders nothing.
	ReadyMessage string
	// StopAt is the planned stop time, zero when unknown.
	StopAt time.Time
}

// Event describes one operation on one cluster.
type Event struct {
	Kind Kind
	// Env and ClusterName identify the cluster; the notify channel is read from the live registry by Env.
	Env         string
	ClusterName string
	// Namespace is the namespace of the operation; empty when it spans namespaces (stop_all).
	Namespace string
	// Actor is the display text of who did it, for example "<@123>", "nightly-stop" or "api".
	Actor string
	// StartedBy is the raw starter recorded in the store, exposed to ready message templates.
	StartedBy string
	Workloads []Workload
}

// Notifier is what operation sources call. Every method is a no-op for a cluster without a notify channel
// and never returns an error: a failed post is logged and the operation goes on.
type Notifier interface {
	// Enabled reports whether a notify channel is configured for env.
	Enabled(env string) bool
	// Started posts "start requested" and remembers the workloads so Ready can edit the message.
	Started(ctx context.Context, e Event)
	// Stopped posts a stop or stop_all message.
	Stopped(ctx context.Context, e Event)
	// StopReminder posts "<name> stops at <time>" with an Extend 1h button.
	StopReminder(ctx context.Context, w model.Workload, stopAt time.Time)
}

// Nop is a Notifier that does nothing.
type Nop struct{}

func (Nop) Enabled(string) bool                                     { return false }
func (Nop) Started(context.Context, Event)                          {}
func (Nop) Stopped(context.Context, Event)                          {}
func (Nop) StopReminder(context.Context, model.Workload, time.Time) {}

// Button is an action button under a message. Payload comes back to the adapter when it is pressed.
type Button struct {
	Label   string
	Payload string
}

// Message is a platform-neutral chat message.
type Message struct {
	Text    string
	Buttons []Button
}

// Poster posts and edits messages on one chat platform.
type Poster interface {
	// Platform is the frontend name (config.FrontendSlack or config.FrontendDiscord).
	Platform() string
	// Post sends m to the channel and returns the reference needed to edit it.
	Post(ctx context.Context, channel string, m Message) (ref string, err error)
	// Edit replaces the message posted earlier.
	Edit(ctx context.Context, channel, ref string, m Message) error
}

// ClusterResolver looks the live cluster up by env, so a hot reloaded notify channel applies at once.
type ClusterResolver interface {
	ByEnv(env string) (config.Cluster, bool)
}

const (
	// callTimeout bounds one post or edit.
	callTimeout = 5 * time.Second
	// defaultEditDelay coalesces ready edits of one operation into one chat API call.
	defaultEditDelay = time.Second
	// pendingTTL drops workloads that never became ready (rollout gave up or the stop came first).
	pendingTTL = 30 * time.Minute
	// maxListed caps the workloads named in one message.
	maxListed = 20
)

// Service implements Notifier on top of the registered Posters.
type Service struct {
	clusters  ClusterResolver
	editDelay time.Duration
	now       func() time.Time

	mu      sync.Mutex
	posters map[string]Poster
	pending map[string]pendingWorkload
}

type pendingWorkload struct {
	op       *startOp
	workload Workload
	at       time.Time
}

// Option customizes a Service.
type Option func(*Service)

// WithEditDelay sets how long ready edits are coalesced; 0 edits immediately (tests).
func WithEditDelay(d time.Duration) Option { return func(s *Service) { s.editDelay = d } }

// New builds a Service reading notify channels from clusters.
func New(clusters ClusterResolver, opts ...Option) *Service {
	s := &Service{
		clusters:  clusters,
		editDelay: defaultEditDelay,
		now:       time.Now,
		posters:   map[string]Poster{},
		pending:   map[string]pendingWorkload{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ Notifier = (*Service)(nil)

// AddPoster registers the adapter of one chat platform; a second one for the same platform replaces it.
func (s *Service) AddPoster(p Poster) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posters[p.Platform()] = p
}

type target struct {
	poster  Poster
	channel string
}

// targets returns the posters with a notify channel configured for env in the live registry.
func (s *Service) targets(env string) []target {
	cl, ok := s.clusters.ByEnv(env)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []target
	for _, name := range []string{config.FrontendDiscord, config.FrontendSlack} {
		p, ok := s.posters[name]
		if ch := cl.NotifyChannel(name); ok && ch != "" {
			out = append(out, target{poster: p, channel: ch})
		}
	}
	return out
}

// Enabled implements Notifier.
func (s *Service) Enabled(env string) bool { return len(s.targets(env)) > 0 }

func key(env, namespace, name string) string { return env + "/" + namespace + "/" + name }

func (s *Service) post(ctx context.Context, t target, m Message) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	ref, err := t.poster.Post(ctx, t.channel, m)
	if err != nil {
		slog.Warn("notify post failed", slog.String("platform", t.poster.Platform()), slog.String("error.message", err.Error()))
		return "", false
	}
	return ref, true
}

// Started implements Notifier.
func (s *Service) Started(ctx context.Context, e Event) {
	targets := s.targets(e.Env)
	if len(targets) == 0 || len(e.Workloads) == 0 {
		return
	}
	op := &startOp{svc: s, event: e, ready: map[string]string{}}
	msg := Message{Text: op.render()}
	for _, t := range targets {
		if ref, ok := s.post(ctx, t, msg); ok {
			op.posts = append(op.posts, post{target: t, ref: ref})
		}
	}
	if len(op.posts) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, p := range s.pending {
		if now.Sub(p.at) > pendingTTL {
			delete(s.pending, k)
		}
	}
	for _, w := range e.Workloads {
		s.pending[key(e.Env, w.Namespace, w.Name)] = pendingWorkload{op: op, workload: w, at: now}
	}
}

// Ready edits the start message of a workload that became ready. It is a no-op for workloads
// that were not announced with Started (no channel, or started by another replica).
func (s *Service) Ready(env, namespace, name string) {
	k := key(env, namespace, name)
	s.mu.Lock()
	p, ok := s.pending[k]
	delete(s.pending, k)
	s.mu.Unlock()
	if ok {
		p.op.markReady(p.workload)
	}
}

// Stopped implements Notifier.
func (s *Service) Stopped(ctx context.Context, e Event) {
	s.mu.Lock()
	for _, w := range e.Workloads {
		delete(s.pending, key(e.Env, w.Namespace, w.Name))
	}
	s.mu.Unlock()

	targets := s.targets(e.Env)
	if len(targets) == 0 || len(e.Workloads) == 0 {
		return
	}
	msg := Message{Text: stopText(e)}
	for _, t := range targets {
		s.post(ctx, t, msg)
	}
}

// Announce posts a plain text message to the notify channels of env (reservation reminders and misses).
func (s *Service) Announce(ctx context.Context, env, text string) {
	for _, t := range s.targets(env) {
		s.post(ctx, t, Message{Text: text})
	}
}

// StopReminder implements Notifier.
func (s *Service) StopReminder(ctx context.Context, w model.Workload, stopAt time.Time) {
	targets := s.targets(w.Env)
	if len(targets) == 0 {
		return
	}
	msg := Message{
		Text:    fmt.Sprintf("⏰ %s stops at %s (%s / %s)", w.Name, stopAt.Format("15:04 MST"), strings.ToUpper(w.Env), w.Namespace),
		Buttons: []Button{{Label: "Extend 1h", Payload: EncodeExtend(WorkloadRef{Env: w.Env, Namespace: w.Namespace, Name: w.Name})}},
	}
	for _, t := range targets {
		s.post(ctx, t, msg)
	}
}

type post struct {
	target target
	ref    string
}

// startOp is the one message of a start operation, edited as workloads become ready.
type startOp struct {
	svc   *Service
	event Event
	posts []post

	mu        sync.Mutex
	ready     map[string]string // workload name -> rendered ready message ("" when none)
	scheduled bool
}

func (o *startOp) header() string {
	e := o.event
	return fmt.Sprintf("🚀 %s requested start (%s / %s / %s)", e.Actor, strings.ToUpper(e.Env), e.ClusterName, e.Namespace)
}

// render must be called with o.mu held, or before the op is shared.
func (o *startOp) render() string {
	lines := make([]string, 0, len(o.event.Workloads))
	for _, w := range o.event.Workloads {
		msg, done := o.ready[w.Name]
		switch {
		case !done:
			lines = append(lines, fmt.Sprintf("⏳ %s starting...", w.Name))
		case msg == "":
			lines = append(lines, fmt.Sprintf("✅ %s started", w.Name))
		default:
			lines = append(lines, fmt.Sprintf("✅ %s started — %s", w.Name, msg))
		}
	}
	return joinLines(o.header(), lines)
}

func (o *startOp) markReady(w Workload) {
	line, err := RenderReady(w.ReadyMessage, ReadyData{
		Name: w.Name, Namespace: w.Namespace, Cluster: o.event.Env,
		StartedBy: o.event.StartedBy, StopAt: FormatStopAt(w.StopAt),
	})
	if err != nil {
		slog.Warn("ready-message render failed, the plain ready line is used", slog.String("cluster.env", o.event.Env), slog.String("namespace", w.Namespace), slog.String("workload", w.Name), slog.String("error.message", err.Error()))
		line = ""
	}
	o.mu.Lock()
	o.ready[w.Name] = line
	if o.svc.editDelay <= 0 {
		o.flushLocked()
		o.mu.Unlock()
		return
	}
	if !o.scheduled {
		o.scheduled = true
		time.AfterFunc(o.svc.editDelay, o.flush)
	}
	o.mu.Unlock()
}

func (o *startOp) flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.scheduled = false
	o.flushLocked()
}

func (o *startOp) flushLocked() {
	msg := Message{Text: o.render()}
	for _, p := range o.posts {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		if err := p.target.poster.Edit(ctx, p.target.channel, p.ref, msg); err != nil {
			slog.Warn("notify edit failed", slog.String("platform", p.target.poster.Platform()), slog.String("error.message", err.Error()))
		}
		cancel()
	}
}

func stopText(e Event) string {
	names := make([]string, 0, len(e.Workloads))
	for _, w := range e.Workloads {
		if e.Namespace == "" {
			names = append(names, w.Namespace+"/"+w.Name)
		} else {
			names = append(names, w.Name)
		}
	}
	slices.Sort(names)
	where := fmt.Sprintf("%s / %s", strings.ToUpper(e.Env), e.ClusterName)
	if e.Namespace != "" {
		where += " / " + e.Namespace
	}
	return joinLines(fmt.Sprintf("⏹ %s stopped workloads (%s)", e.Actor, where), prefixAll("⏹ ", names))
}

func prefixAll(prefix string, ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = prefix + s
	}
	return out
}

// joinLines joins head and lines, replacing lines beyond maxListed with a count.
func joinLines(head string, lines []string) string {
	var b strings.Builder
	b.WriteString(head)
	for i, l := range lines {
		if i == maxListed {
			fmt.Fprintf(&b, "\n… and %d more", len(lines)-i)
			break
		}
		b.WriteString("\n" + l)
	}
	return b.String()
}
