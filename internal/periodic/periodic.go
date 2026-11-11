// Package periodic wires every hook the background reconcile loop runs each interval: the
// auto-stop with its stop reminder, and the one-off reservations. New scheduled behaviour (for
// example recurring reservations) is added here so the loop has one list to run.
package periodic

import (
	"github.com/murasame29/workload-knocker/internal/autostop"
	"github.com/murasame29/workload-knocker/internal/reconcile"
	"github.com/murasame29/workload-knocker/internal/reservation"
)

// Hooks are the periodic hooks in the order the loop runs them each tick.
type Hooks []reconcile.Hook

// New returns the hooks in run order: auto-stop first, so a workload whose run ended is stopped
// before a reservation of the same tick may start it again.
func New(autoStop *autostop.Hook, reservations *reservation.Service) Hooks {
	var hooks Hooks
	if autoStop != nil {
		hooks = append(hooks, autoStop)
	}
	if reservations != nil {
		hooks = append(hooks, reservations)
	}
	return hooks
}
