package frontend

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

var testMarkup = Markup{
	Bold:     func(s string) string { return "<b>" + s + "</b>" },
	Time:     func(t time.Time) string { return "T" + t.UTC().Format("15:04") },
	Relative: func(t time.Time) string { return "R" + t.UTC().Format("15:04") },
	Text:     func(s string) string { return "[" + s + "]" },
	Starter:  func(s string) string { return "@" + s },
}

func TestMarkup_Lines(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)

	if got := testMarkup.PlanLines([]Change{{Action: ActionStart, Label: "a"}, {Action: ActionStop, Label: "b"}}); !slices.Equal(got, []string{"🟢 a: stopped → <b>start</b>", "🔴 b: running → <b>stop</b>"}) {
		t.Errorf("plan = %q", got)
	}
	starts := []Schedule{{Name: "a", Duration: time.Hour, StopAt: at}, {Name: "b", Duration: time.Hour, StopAt: at}, {Name: "c", Err: errors.New("too long")}}
	if got := testMarkup.ScheduleLines(starts); !slices.Equal(got, []string{"⏰ a, b: auto-stop in 1h (T21:00)", "⛔ c: too long"}) {
		t.Errorf("schedule = %q", got)
	}
	if got := testMarkup.ScheduleLines([]Schedule{{Name: "a"}}); got != nil {
		t.Errorf("no auto-stop at all must render nothing, got %q", got)
	}
	if got := testMarkup.AppliedStopLines([]Applied{{Name: "a", Found: true, StopAt: at}, {Name: "gone", StopAt: at}}); !slices.Equal(got, []string{"⏰ a: auto-stop at T21:00"}) {
		t.Errorf("applied = %q", got)
	}

	running := StatusEntry{Entry: Entry{Key: "web", Members: []*model.Workload{{Name: "web", Replicas: 1}}}, StartedBy: "alice", StartedAt: at, StopAt: at.Add(time.Hour)}
	if got := testMarkup.StatusLine(running); got != "[🟢 web (1 pod)] · by @alice · started R21:00 · stops R22:00" {
		t.Errorf("status = %q", got)
	}

	r := reservation.Reservation{Namespace: "games", Workloads: []string{"terraria"}, At: at, Duration: 3 * time.Hour, Creator: reservation.Creator{Platform: authz.PlatformDiscord, User: "1"}}
	if got := testMarkup.ReservedLine(r, 5*time.Minute); got != "📅 [games] / [terraria]: ready by T21:00 (starts 5m earlier), auto-stop 3h later" {
		t.Errorf("reserved = %q", got)
	}
	if got := testMarkup.PendingLine(r); got != "📅 [games] / [terraria] · ready by T21:00 · by <@1>" {
		t.Errorf("pending = %q", got)
	}
}

func TestShortDurationAndReservationError(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{5 * time.Minute: "5m", time.Hour: "1h", 90 * time.Minute: "1h30m", 30 * time.Second: "30s"} {
		if got := ShortDuration(d); got != want {
			t.Errorf("ShortDuration(%s) = %q, want %q", d, got, want)
		}
	}
	if got := ReservationError(fmt.Errorf("%w: bad time", reservation.ErrInvalid)); got != "invalid reservation: bad time" {
		t.Errorf("invalid = %q", got)
	}
	if got := ReservationError(errors.New("etcd is down")); got != "internal error, please try again" {
		t.Errorf("internal = %q", got)
	}
}
