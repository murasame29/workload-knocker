// Package rollout follows start and stop operations until the workloads are ready or stopped.
// It is chat-platform neutral; adapters render the snapshots it emits.
package rollout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/murasame29/workload-knocker/internal/k8s"
)

// State is the rollout state of one workload.
type State string

const (
	StateStarting State = "starting"
	StateStarted  State = "started"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	// StateTimeout: the target state was not reached within the watch timeout.
	StateTimeout State = "timeout"
	// StateUntracked: the scale target kind cannot be read, so progress is not followed.
	StateUntracked State = "untracked"
)

// Terminal reports whether the state is final.
func (s State) Terminal() bool {
	return s == StateStarted || s == StateStopped || s == StateTimeout || s == StateUntracked
}

// Direction is whether a workload is being started or stopped.
type Direction string

const (
	Start Direction = "start"
	Stop  Direction = "stop"
)

// Target is one workload to follow.
type Target struct {
	Env       string
	Namespace string
	Name      string
	// Kind is the scale target kind (Deployment, StatefulSet).
	Kind      string
	Direction Direction
}

// Item is the observed progress of one target.
type Item struct {
	Target Target
	State  State
	// Detail is the last observed reason, set on timeout (for example "0/1 ready, ImagePullBackOff").
	Detail string
}

// Snapshot is the progress of all targets of one operation, in the order they were given.
type Snapshot []Item

// Done reports whether every item is final.
func (s Snapshot) Done() bool {
	for _, it := range s {
		if !it.State.Terminal() {
			return false
		}
	}
	return true
}

// ReaderSource returns the reader of the cluster with the given env.
type ReaderSource interface {
	Reader(ctx context.Context, env string) (k8s.RolloutReader, error)
}

// Clock abstracts waiting so tests run without real time.
type Clock interface {
	Now() time.Time
	// Sleep waits for d and returns false when ctx ended first.
	Sleep(ctx context.Context, d time.Duration) bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// issueLookupTimeout bounds the pod lookup done once when a watch times out.
const issueLookupTimeout = 5 * time.Second

// Tracker polls scale targets until they reach the requested state or the timeout passes.
type Tracker struct {
	readers   ReaderSource
	interval  time.Duration
	timeout   time.Duration
	clock     Clock
	onStarted func(Target)
}

// Option customizes a Tracker.
type Option func(*Tracker)

// WithOnStarted registers a hook called once for each workload that becomes started.
func WithOnStarted(fn func(Target)) Option { return func(t *Tracker) { t.onStarted = fn } }

// WithClock replaces the clock (tests).
func WithClock(c Clock) Option { return func(t *Tracker) { t.clock = c } }

// NewTracker builds a Tracker polling every interval and giving up after timeout.
func NewTracker(readers ReaderSource, interval, timeout time.Duration, opts ...Option) *Tracker {
	t := &Tracker{readers: readers, interval: interval, timeout: timeout, clock: realClock{}}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Timeout is the give-up duration, for rendering "not ready after 10m".
func (t *Tracker) Timeout() time.Duration { return t.timeout }

// InitialSnapshot is the starting/stopping snapshot Watch emits first.
func InitialSnapshot(targets []Target) Snapshot {
	s := make(Snapshot, len(targets))
	for i, tg := range targets {
		s[i] = Item{Target: tg, State: StateStarting}
		if tg.Direction == Stop {
			s[i].State = StateStopping
		}
	}
	return s
}

// Watch emits the initial snapshot, then one snapshot per poll in which a state changed, and
// returns when every target is final or ctx ends. onChange is never called concurrently.
func (t *Tracker) Watch(ctx context.Context, targets []Target, onChange func(Snapshot)) {
	snap := InitialSnapshot(targets)
	onChange(append(Snapshot(nil), snap...))
	deadline := t.clock.Now().Add(t.timeout)

	// Sleep first: KEDA reacts after its own polling interval, and this caps edits at one per interval.
	for !snap.Done() && t.clock.Sleep(ctx, t.interval) {
		changed := false
		timedOut := !t.clock.Now().Before(deadline)
		for i := range snap {
			if snap[i].State.Terminal() {
				continue
			}
			state, reason := t.observe(ctx, snap[i].Target)
			switch {
			case state.Terminal():
				snap[i].State = state
			case timedOut:
				snap[i].State, snap[i].Detail = StateTimeout, t.timeoutDetail(snap[i].Target, reason)
			default:
				continue
			}
			changed = true
			if snap[i].State == StateStarted && t.onStarted != nil {
				t.onStarted(snap[i].Target)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if changed {
			onChange(append(Snapshot(nil), snap...))
		}
	}
}

// observe reads the target once. It returns the final state when reached, else the current
// progress state and a reason such as "0/1 ready".
func (t *Tracker) observe(ctx context.Context, tg Target) (State, string) {
	progress := StateStarting
	if tg.Direction == Stop {
		progress = StateStopping
	}
	r, err := t.readers.Reader(ctx, tg.Env)
	if err != nil {
		return progress, "cluster unavailable: " + err.Error()
	}
	st, err := r.Replicas(ctx, tg.Kind, tg.Namespace, tg.Name)
	if errors.Is(err, k8s.ErrUnsupportedKind) {
		return StateUntracked, err.Error()
	}
	if err != nil {
		slog.Debug("rollout poll failed", slog.String("cluster.env", tg.Env), slog.String("namespace", tg.Namespace), slog.String("workload", tg.Name), slog.String("error.message", err.Error()))
		return progress, "status unavailable: " + err.Error()
	}
	reason := fmt.Sprintf("%d/%d ready", st.Ready, st.Desired)
	if tg.Direction == Stop {
		if st.Desired == 0 && st.Ready == 0 {
			return StateStopped, reason
		}
		return progress, reason
	}
	switch {
	case st.Desired == 0:
		return progress, "0 replicas (waiting for KEDA to scale up)"
	case st.Ready >= st.Desired && st.Ready > 0:
		return StateStarted, reason
	}
	return progress, reason
}

// timeoutDetail appends the pod issue (for example ImagePullBackOff) to the last progress reason.
func (t *Tracker) timeoutDetail(tg Target, reason string) string {
	if tg.Direction == Stop {
		return reason
	}
	ctx, cancel := context.WithTimeout(context.Background(), issueLookupTimeout)
	defer cancel()
	r, err := t.readers.Reader(ctx, tg.Env)
	if err != nil {
		return reason
	}
	st, err := r.Replicas(ctx, tg.Kind, tg.Namespace, tg.Name)
	if err != nil {
		return reason
	}
	issue, err := r.PodIssue(ctx, tg.Namespace, st.Selector)
	if err != nil || issue == "" {
		return reason
	}
	return strings.TrimSpace(reason + ", " + issue)
}
