// Package reservation holds one-off reservations: workloads that must be Ready at a requested
// time. The background reconcile loop starts them warmup ahead of that time (see Service.Tick);
// reservations persist in the state ConfigMap so they survive restarts.
package reservation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/murasame29/workload-knocker/internal/authz"
)

// State is the lifecycle state of a reservation.
type State string

const (
	// Pending waits for its start time.
	Pending State = "pending"
	// Started had at least one workload started.
	Started State = "started"
	// Failed reached its start time but no workload could be started; see Note.
	Failed State = "failed"
	// Missed was not run because the controller was down past the grace period.
	Missed State = "missed"
	// Cancelled was cancelled before its start time.
	Cancelled State = "cancelled"
)

// States lists every state, for metrics.
var States = []State{Pending, Started, Failed, Missed, Cancelled}

// Reservation is one requested start.
type Reservation struct {
	ID        string   `json:"id"`
	Namespace string   `json:"namespace"`
	Workloads []string `json:"workloads"`
	// At is when the workloads should be Ready; they start Warmup earlier.
	At time.Time `json:"at"`
	// Duration is the requested run length after At; 0 uses each workload's default.
	Duration  time.Duration `json:"duration,omitempty"`
	Creator   Creator       `json:"creator"`
	CreatedAt time.Time     `json:"createdAt"`
	State     State         `json:"state"`
	// Reminded is set once the reminder was posted.
	Reminded bool `json:"reminded,omitempty"`
	// Note explains a Failed or Missed state, or lists the workloads that could not start.
	Note string `json:"note,omitempty"`
	// DoneAt is when the reservation left Pending; finished ones are dropped after a retention.
	DoneAt time.Time `json:"doneAt,omitzero"`
}

// Creator is who made the reservation; authorization is checked again with it at execution.
type Creator struct {
	Platform authz.Platform `json:"platform"`
	User     string         `json:"user,omitempty"`
	// Groups are the roles or user groups at creation (chat platforms send them with each
	// interaction only, so execution re-checks with these).
	Groups []string `json:"groups,omitempty"`
}

// CreatorOf returns the Creator of a subject.
func CreatorOf(s authz.Subject) Creator {
	return Creator{Platform: s.Platform, User: s.User, Groups: slices.Clone(s.Groups)}
}

// Subject rebuilds the authorization subject of the creator.
func (c Creator) Subject() authz.Subject {
	return authz.Subject{Platform: c.Platform, User: c.User, Groups: slices.Clone(c.Groups)}
}

// Is reports whether s is the creator.
func (c Creator) Is(s authz.Subject) bool { return c.Platform == s.Platform && c.User == s.User }

// Mention is the chat display of the creator ("<@id>" on chat, "api" for the REST API).
func (c Creator) Mention() string {
	if c.User == "" || c.Platform == authz.PlatformAPI {
		return "api"
	}
	return "<@" + c.User + ">"
}

// Errors returned by Service.Create and Service.Cancel; the message is safe to show in a chat.
var (
	ErrInvalid   = errors.New("invalid reservation")
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("not authorized")
)

func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
