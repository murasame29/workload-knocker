package rollout

import (
	"fmt"
	"time"
)

// Line renders one item as a chat line; timeout is the watch timeout shown on give-up.
func Line(it Item, timeout time.Duration) string { return LineWith(fmt.Sprintf, it, timeout) }

// LineWith is Line with printf formatting every message, so a caller can translate the formats.
func LineWith(printf func(format string, args ...any) string, it Item, timeout time.Duration) string {
	n := it.Target.Name
	switch it.State {
	case StateStarted:
		if it.Detail != "" {
			return printf("✅ %s started — %s", n, it.Detail)
		}
		return printf("✅ %s started", n)
	case StateStopped:
		return printf("⏹ %s stopped", n)
	case StateStopping:
		return printf("⏳ %s stopping...", n)
	case StateTimeout:
		if it.Target.Direction == Stop {
			return printf("⚠ %s still running after %s (%s)", n, FormatDuration(timeout), it.Detail)
		}
		return printf("⚠ %s not ready after %s (%s)", n, FormatDuration(timeout), it.Detail)
	case StateUntracked:
		return printf("ℹ %s applied (progress is not tracked for kind %s)", n, it.Target.Kind)
	}
	return printf("⏳ %s starting...", n)
}

// FormatDuration renders whole minutes as "10m" and everything else with the standard format.
func FormatDuration(d time.Duration) string {
	if d > 0 && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
