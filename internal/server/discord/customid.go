package discordbot

import (
	"fmt"
	"strconv"
	"strings"
)

// customIDVersion prefixes every custom_id so a future layout can be told apart from this one.
const customIDVersion = "wk1"

// Component kinds carried in the custom_id.
const (
	kindNamespaceSelect = "ns"  // namespace select menu
	kindNamespacePage   = "nsp" // namespace page button (Arg = target page)
	kindWorkloadSelect  = "wl"  // workload multi-select menu
	kindWorkloadPage    = "wlp" // workload page button (Arg = target page)
	kindPlan            = "ap"  // Apply button: show the plan preview
	kindConfirm         = "ok"  // Confirm button: apply the plan
	kindCancel          = "no"  // Cancel button
	kindDuration        = "du"  // Change duration button: opens the duration modal
	kindDurationRetry   = "dr"  // Try again button after an invalid duration: reopens the modal
	kindDurationModal   = "dm"  // duration modal submission
	kindExtend          = "ex"  // Extend button of a stop reminder (Token keys the button payload)
	kindStatusExtend    = "sx"  // Extend 1h button of a status entry (Token keys the button payload)
	kindStatusStop      = "ss"  // Stop button of a status entry (Token keys the button payload)
	kindNamespaces      = "nsl" // Other namespace button: back to the namespace menu, selections kept
	kindSelectModal     = "sm"  // workload selection modal submission (one checkbox group per namespace)
	kindScheduleModal   = "rm"  // reservation modal submission
	kindReservationStop = "rc"  // Cancel button of a reservation in status (Token keys the button payload)
)

// customID is the decoded component identifier: wk1:<kind>:<token>:<arg>.
// Token keys the in-memory flow state; Arg is the target page for page buttons, else 0.
type customID struct {
	Kind  string
	Token string
	Arg   int
}

func (c customID) String() string {
	return fmt.Sprintf("%s:%s:%s:%d", customIDVersion, c.Kind, c.Token, c.Arg)
}

// parseCustomID decodes a custom_id built by customID.String.
func parseCustomID(s string) (customID, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 || parts[0] != customIDVersion {
		return customID{}, fmt.Errorf("unrecognized custom_id %q", s)
	}
	arg, err := strconv.Atoi(parts[3])
	if err != nil || arg < 0 {
		return customID{}, fmt.Errorf("invalid custom_id arg in %q", s)
	}
	if parts[1] == "" || parts[2] == "" {
		return customID{}, fmt.Errorf("empty custom_id field in %q", s)
	}
	return customID{Kind: parts[1], Token: parts[2], Arg: arg}, nil
}
