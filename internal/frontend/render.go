package frontend

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/murasame29/workload-knocker/internal/reservation"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// Markup is how a chat platform formats the shared lines; adapters (Discord, Slack) set it once
// and render plans, results, schedules and status through it.
type Markup struct {
	// Bold emphasises a word ("**x**" on Discord, "*x*" on Slack).
	Bold func(string) string
	// Time renders an absolute time; Relative renders "in 2 hours" style times.
	Time     func(time.Time) string
	Relative func(time.Time) string
	// Text escapes user-controlled text (workload labels, starters) for the platform.
	Text func(string) string
	// Starter renders who started a workload (already escaped by the implementation).
	Starter func(string) string
}

// PlanLines renders plan changes.
func (m Markup) PlanLines(changes []Change) []string {
	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Action == ActionStart {
			lines = append(lines, fmt.Sprintf("🟢 %s: stopped → %s", c.Label, m.Bold("start")))
		} else {
			lines = append(lines, fmt.Sprintf("🔴 %s: running → %s", c.Label, m.Bold("stop")))
		}
	}
	return lines
}

// RejectionLines renders refused changes with the reason.
func RejectionLines(rejected []Rejection) []string {
	lines := make([]string, 0, len(rejected))
	for _, r := range rejected {
		lines = append(lines, fmt.Sprintf("⛔ %s: %s", r.Label, r.Message()))
	}
	return lines
}

// ScheduleLines groups the starts by run duration; nothing is shown when no start has an auto-stop.
func (m Markup) ScheduleLines(starts []Schedule) []string {
	var scheduled bool
	for _, s := range starts {
		scheduled = scheduled || s.Duration > 0
	}
	if !scheduled {
		return nil
	}
	var lines []string
	done := map[time.Duration]bool{}
	for _, s := range starts {
		if s.Err != nil {
			lines = append(lines, fmt.Sprintf("⛔ %s: %v", s.Name, s.Err))
			continue
		}
		if done[s.Duration] {
			continue
		}
		done[s.Duration] = true
		var names []string
		for _, o := range starts {
			if o.Err == nil && o.Duration == s.Duration {
				names = append(names, o.Name)
			}
		}
		if s.Duration == 0 {
			lines = append(lines, fmt.Sprintf("⏰ %s: no auto-stop", strings.Join(names, ", ")))
		} else {
			lines = append(lines, fmt.Sprintf("⏰ %s: auto-stop in %s (%s)", strings.Join(names, ", "), runlimit.FormatHours(s.Duration), m.Time(s.StopAt)))
		}
	}
	return lines
}

// AppliedStopLines groups the started workloads by their stop time.
func (m Markup) AppliedStopLines(applied []Applied) []string {
	var lines []string
	done := map[int64]bool{}
	for _, a := range applied {
		if !a.Found || a.StopAt.IsZero() || done[a.StopAt.Unix()] {
			continue
		}
		done[a.StopAt.Unix()] = true
		var names []string
		for _, o := range applied {
			if o.Found && o.StopAt.Unix() == a.StopAt.Unix() {
				names = append(names, o.Name)
			}
		}
		lines = append(lines, fmt.Sprintf("⏰ %s: auto-stop at %s", strings.Join(names, ", "), m.Time(a.StopAt)))
	}
	return lines
}

// StatusLine renders one status entry: its state label, then who started it, when, and its stop time.
func (m Markup) StatusLine(se StatusEntry) string {
	line := m.Text(se.Entry.Label())
	if !se.Entry.AnyRunning() {
		return line
	}
	if se.StartedBy != "" {
		line += " · by " + m.Starter(se.StartedBy)
	}
	if !se.StartedAt.IsZero() {
		line += " · started " + m.Relative(se.StartedAt)
	}
	if se.StopAt.IsZero() {
		return line + " · no auto-stop"
	}
	return line + " · stops " + m.Relative(se.StopAt)
}

// ReservedLine renders a created reservation with its warmup and run duration.
func (m Markup) ReservedLine(r reservation.Reservation, warmup time.Duration) string {
	line := fmt.Sprintf("📅 %s / %s: ready by %s (starts %s earlier)", m.Text(r.Namespace), m.Text(strings.Join(r.Workloads, ", ")), m.Time(r.At), ShortDuration(warmup))
	if r.Duration > 0 {
		line += ", auto-stop " + runlimit.FormatHours(r.Duration) + " later"
	}
	return line
}

// PendingLine renders a pending reservation of the status list.
func (m Markup) PendingLine(r reservation.Reservation) string {
	return fmt.Sprintf("📅 %s / %s · ready by %s · by %s", m.Text(r.Namespace), m.Text(strings.Join(r.Workloads, ", ")), m.Time(r.At), r.Creator.Mention())
}

// CancelledLine renders the note of a cancelled reservation.
func (m Markup) CancelledLine(r reservation.Reservation) string {
	return fmt.Sprintf("🗑 Cancelled the reservation of %s / %s", m.Text(r.Namespace), m.Text(strings.Join(r.Workloads, ", ")))
}

// ShortDuration renders d like "5m" or "1h30m".
func ShortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// ReservationError is the chat text of a reservation error; internal errors stay generic.
func ReservationError(err error) string {
	if errors.Is(err, reservation.ErrInvalid) || errors.Is(err, reservation.ErrForbidden) || errors.Is(err, reservation.ErrNotFound) {
		return err.Error()
	}
	slog.Error("reservation request failed", slog.String("error.message", err.Error()))
	return "internal error, please try again"
}
