// Package audit emits one structured audit event per workload per operation.
// The schema is documented in docs/en/audit-log.md.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
)

// Message is the fixed log message of every audit event.
const Message = "workload operation"

// Event is the operation kind (event.name).
type Event string

const (
	WorkloadStart      Event = "workload.start"
	WorkloadStop       Event = "workload.stop"
	WorkloadStopAll    Event = "workload.stop_all"
	WorkloadExtend     Event = "workload.extend"
	WorkloadAutoStop   Event = "workload.auto_stop"
	ReservationCreate  Event = "reservation.create"
	ReservationCancel  Event = "reservation.cancel"
	ReservationExecute Event = "reservation.execute"
)

// Outcome is the result of the operation (event.outcome).
type Outcome string

const (
	Success Outcome = "success"
	Failure Outcome = "failure"
	// Denied means the authorization policy or the operation mode refused the operation.
	Denied Outcome = "denied"
)

// Source is where the operation came from (workload_knocker.source).
type Source string

const (
	SourceDiscord     Source = "discord"
	SourceSlack       Source = "slack"
	SourceAPI         Source = "api"
	SourceScheduler   Source = "scheduler"
	SourceNightlyStop Source = "nightly-stop"
)

// EndUserAPI is the enduser.id of every REST API call.
const EndUserAPI = "api"

// EndUser formats enduser.id as "<source>:<id>"; an empty id yields the bare source.
func EndUser(source Source, id string) string {
	if id == "" {
		return string(source)
	}
	return string(source) + ":" + id
}

// Record is one audit event. Empty and zero fields are omitted from the log line.
type Record struct {
	Event     Event
	Outcome   Outcome
	Source    Source
	RequestID string
	EndUser   string

	Cluster      string
	Namespace    string
	WorkloadName string
	WorkloadKind string

	Duration time.Duration
	StopAt   time.Time

	Err error
}

// Recorder writes audit events.
type Recorder interface {
	Record(ctx context.Context, r Record)
}

// NewSlog returns a Recorder writing to the default slog logger at call time.
func NewSlog() Recorder { return slogRecorder{} }

type slogRecorder struct{}

func (slogRecorder) Record(ctx context.Context, r Record) {
	level := slog.LevelInfo
	if r.Outcome == Failure {
		level = slog.LevelWarn
	}
	slog.LogAttrs(ctx, level, Message, r.attrs()...)
}

func (r Record) attrs() []slog.Attr {
	a := []slog.Attr{
		slog.String("event.name", string(r.Event)),
		slog.String("event.outcome", string(r.Outcome)),
		slog.String("workload_knocker.source", string(r.Source)),
		slog.String("workload_knocker.request_id", r.RequestID),
		slog.String("enduser.id", r.EndUser),
	}
	opt := func(key, v string) {
		if v != "" {
			a = append(a, slog.String(key, v))
		}
	}
	opt("k8s.cluster.name", r.Cluster)
	opt("k8s.namespace.name", r.Namespace)
	opt("workload.name", r.WorkloadName)
	opt("workload.kind", r.WorkloadKind)
	if r.Duration > 0 {
		a = append(a, slog.Int64("workload_knocker.duration_seconds", int64(r.Duration/time.Second)))
	}
	if !r.StopAt.IsZero() {
		a = append(a, slog.String("workload_knocker.stop_at", r.StopAt.UTC().Format(time.RFC3339)))
	}
	if r.Err != nil {
		a = append(a, slog.String("error.type", fmt.Sprintf("%T", r.Err)), slog.String("error.message", r.Err.Error()))
	}
	return a
}

// NewRequestID returns a random 128-bit hex ID that ties the preview and the confirm of one operation.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("audit: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}
