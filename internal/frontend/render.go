package frontend

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/murasame29/workload-knocker/internal/frontend/i18n"
	"github.com/murasame29/workload-knocker/internal/policy/runlimit"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
	"github.com/murasame29/workload-knocker/internal/workload/rollout"
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
	// Lang is the language of the lines; empty means English.
	Lang i18n.Lang
}

func (m Markup) t(format string, args ...any) string { return m.Lang.T(format, args...) }

// PlanLines renders plan changes.
func (m Markup) PlanLines(changes []Change) []string {
	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Action == ActionStart {
			lines = append(lines, m.t("🟢 %s: stopped → %s", c.Label, m.Bold(m.t("start"))))
		} else {
			lines = append(lines, m.t("🔴 %s: running → %s", c.Label, m.Bold(m.t("stop"))))
		}
	}
	return lines
}

// RejectionLines renders refused changes with the reason in English.
func RejectionLines(rejected []Rejection) []string { return Markup{}.RejectionLines(rejected) }

// RejectionLines renders refused changes with the reason.
func (m Markup) RejectionLines(rejected []Rejection) []string {
	lines := make([]string, 0, len(rejected))
	for _, r := range rejected {
		lines = append(lines, fmt.Sprintf("⛔ %s: %s", r.Label, r.MessageIn(m.Lang)))
	}
	return lines
}

// RolloutLine renders one rollout item.
func (m Markup) RolloutLine(it rollout.Item, timeout time.Duration) string {
	return rollout.LineWith(m.t, it, timeout)
}

// StopLines renders the outcome of a stop, one line per workload.
func (m Markup) StopLines(r ApplyResult) []string {
	var lines []string
	for _, a := range r.Applied {
		if !a.Found {
			lines = append(lines, m.t("⚠ %s: not found", a.Name))
			continue
		}
		lines = append(lines, m.t("🔴 %s → stop", a.Name))
	}
	return append(lines, m.RejectionLines(r.Rejected)...)
}

// ExtendLines renders the outcome of an extension; format renders the new stop time.
func (m Markup) ExtendLines(r ExtendResult, format func(time.Time) string) []string {
	var lines []string
	for _, x := range r.Extended {
		if x.Err != nil {
			lines = append(lines, fmt.Sprintf("⚠ %s: %v", x.Name, x.Err))
			continue
		}
		lines = append(lines, m.t("⏰ %s now stops at %s", x.Name, format(x.StopAt)))
	}
	return append(lines, m.RejectionLines(r.Rejected)...)
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
			lines = append(lines, m.t("⏰ %s: no auto-stop", strings.Join(names, ", ")))
		} else {
			lines = append(lines, m.t("⏰ %s: auto-stop in %s (%s)", strings.Join(names, ", "), runlimit.FormatHours(s.Duration), m.Time(s.StopAt)))
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
		lines = append(lines, m.t("⏰ %s: auto-stop at %s", strings.Join(names, ", "), m.Time(a.StopAt)))
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
		line += m.t(" · by %s", m.Starter(se.StartedBy))
	}
	if !se.StartedAt.IsZero() {
		line += m.t(" · started %s", m.Relative(se.StartedAt))
	}
	if se.StopAt.IsZero() {
		return line + m.t(" · no auto-stop")
	}
	return line + m.t(" · stops %s", m.Relative(se.StopAt))
}

// ReservedLine renders a created reservation with its warmup and run duration.
func (m Markup) ReservedLine(r reservation.Reservation, warmup time.Duration) string {
	line := m.t("📅 %s / %s: ready by %s (starts %s earlier)", m.Text(r.Namespace), m.Text(strings.Join(r.Workloads, ", ")), m.Time(r.At), ShortDuration(warmup))
	if r.Duration > 0 {
		line += m.t(", auto-stop %s later", runlimit.FormatHours(r.Duration))
	}
	return line
}

// PendingLine renders a pending reservation of the status list.
func (m Markup) PendingLine(r reservation.Reservation) string {
	return m.t("📅 %s / %s · ready by %s · by %s", m.Text(r.Namespace), m.Text(strings.Join(r.Workloads, ", ")), m.Time(r.At), r.Creator.Mention())
}

// CancelledLine renders the note of a cancelled reservation.
func (m Markup) CancelledLine(r reservation.Reservation) string {
	return m.t("🗑 Cancelled the reservation of %s / %s", m.Text(r.Namespace), m.Text(strings.Join(r.Workloads, ", ")))
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

// ReservationError is the chat text of a reservation error in English; internal errors stay generic.
func ReservationError(err error) string { return Markup{}.ReservationError(err) }

// ReservationError is the chat text of a reservation error; internal errors stay generic.
// The detail of a refused reservation comes from the reservation service and stays English.
func (m Markup) ReservationError(err error) string {
	if errors.Is(err, reservation.ErrInvalid) || errors.Is(err, reservation.ErrForbidden) || errors.Is(err, reservation.ErrNotFound) {
		return err.Error()
	}
	slog.Error("reservation request failed", slog.String("error.message", err.Error()))
	return m.t("internal error, please try again")
}
