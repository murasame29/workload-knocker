package rollout

import (
	"fmt"
	"time"
)

// Line renders one item as a chat line; timeout is the watch timeout shown on give-up.
func Line(it Item, timeout time.Duration) string {
	n := it.Target.Name
	switch it.State {
	case StateStarted:
		return fmt.Sprintf("✅ %s started", n)
	case StateStopped:
		return fmt.Sprintf("⏹ %s stopped", n)
	case StateStopping:
		return fmt.Sprintf("⏳ %s stopping...", n)
	case StateTimeout:
		if it.Target.Direction == Stop {
			return fmt.Sprintf("⚠ %s still running after %s (%s)", n, FormatDuration(timeout), it.Detail)
		}
		return fmt.Sprintf("⚠ %s not ready after %s (%s)", n, FormatDuration(timeout), it.Detail)
	case StateUntracked:
		return fmt.Sprintf("ℹ %s applied (progress is not tracked for kind %s)", n, it.Target.Kind)
	}
	return fmt.Sprintf("⏳ %s starting...", n)
}

// FormatDuration renders whole minutes as "10m" and everything else with the standard format.
func FormatDuration(d time.Duration) string {
	if d > 0 && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
