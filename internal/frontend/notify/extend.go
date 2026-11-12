package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/murasame29/workload-knocker/internal/policy/authz"
)

// ExtendBy is how long the Extend button on a stop reminder extends the run.
const ExtendBy = time.Hour

// ErrExtendUnavailable is returned by the default Extender until the run-duration feature is wired in.
var ErrExtendUnavailable = errors.New("extending is not available yet")

// WorkloadRef identifies one workload.
type WorkloadRef struct {
	Env       string
	Namespace string
	Name      string
}

// ExtendRequest is one press of the Extend button.
type ExtendRequest struct {
	// Subject is the user who pressed the button, resolved by the adapter like for any other operation.
	Subject authz.Subject
	Ref     WorkloadRef
	By      time.Duration
}

// Extender extends the planned stop time of a running workload and returns the new stop time.
// The error text is shown to the user who pressed the button.
type Extender interface {
	Extend(ctx context.Context, req ExtendRequest) (time.Time, error)
}

type unavailableExtender struct{}

func (unavailableExtender) Extend(context.Context, ExtendRequest) (time.Time, error) {
	return time.Time{}, ErrExtendUnavailable
}

// UnavailableExtender is the default Extender: every press answers that extending is not available.
func UnavailableExtender() Extender { return unavailableExtender{} }

type extendPayload struct {
	Env       string `json:"e"`
	Namespace string `json:"n"`
	Name      string `json:"w"`
}

// EncodeExtend builds the Button payload of a stop reminder.
func EncodeExtend(ref WorkloadRef) string {
	b, _ := json.Marshal(extendPayload(ref))
	return string(b)
}

// DecodeExtend parses a payload built by EncodeExtend.
func DecodeExtend(payload string) (WorkloadRef, error) {
	var p extendPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Env == "" || p.Namespace == "" || p.Name == "" {
		return WorkloadRef{}, fmt.Errorf("invalid extend payload")
	}
	return WorkloadRef(p), nil
}

// ExtendReply is the ephemeral answer to the user who pressed Extend.
func ExtendReply(ref WorkloadRef, stopAt time.Time, err error) string {
	switch {
	case errors.Is(err, ErrExtendUnavailable):
		return "⚠ Extending is not available yet."
	case err != nil:
		return "⚠ " + err.Error()
	case stopAt.IsZero():
		return fmt.Sprintf("⏰ Extended %s by 1h.", ref.Name)
	}
	return fmt.Sprintf("⏰ Extended %s: it now stops at %s.", ref.Name, stopAt.Format("15:04 MST"))
}
