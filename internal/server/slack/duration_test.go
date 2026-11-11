package slackbot

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/frontend"
)

func TestDurationInputBlock_IsOptionalDecimalNumberInput(t *testing.T) {
	t.Parallel()

	b := durationInputBlock()
	el, ok := b.Element.(*slack.NumberInputBlockElement)
	if !ok {
		t.Fatalf("element = %T, want number input", b.Element)
	}
	if !b.Optional || !el.IsDecimalAllowed || el.MinValue != "0.25" || el.ActionID != actionDuration || b.BlockID != blockDuration {
		t.Errorf("block = %+v element = %+v", b, el)
	}
}

func TestSubmittedDuration(t *testing.T) {
	t.Parallel()

	values := func(v string) map[string]map[string]slack.BlockAction {
		return map[string]map[string]slack.BlockAction{blockDuration: {actionDuration: {Value: v}}}
	}
	for in, want := range map[string]time.Duration{"": 0, "3": 3 * time.Hour, "1.5": 90 * time.Minute} {
		if got, err := submittedDuration(values(in)); err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	if got, err := submittedDuration(nil); err != nil || got != 0 {
		t.Errorf("missing block = %v, %v; want the default", got, err)
	}
	if _, err := submittedDuration(values("x")); err == nil {
		t.Error("non-numeric input must fail")
	}
	if _, err := submittedDuration(values("0.1")); err == nil {
		t.Error("below the minimum must fail")
	}
}

func TestDurationErrors_PointsAtTheDurationBlock(t *testing.T) {
	t.Parallel()

	ok := []frontend.Schedule{{Name: "a", Duration: time.Hour}}
	if durationErrors(ok) != nil {
		t.Error("valid schedules must not produce errors")
	}
	bad := append(ok, frontend.Schedule{Name: "b", Err: errors.New("duration must be at most 8h")})
	errs := durationErrors(bad)
	if msg := errs[blockDuration]; !strings.Contains(msg, "b:") || !strings.Contains(msg, "at most 8h") {
		t.Errorf("errors = %v", errs)
	}
}

func TestScheduleLines(t *testing.T) {
	t.Parallel()

	stop := time.Date(2026, 11, 8, 14, 0, 0, 0, time.UTC)
	if markup.ScheduleLines([]frontend.Schedule{{Name: "a"}}) != nil {
		t.Error("no auto-stop anywhere must add no line")
	}
	lines := markup.ScheduleLines([]frontend.Schedule{
		{Name: "a", Duration: 4 * time.Hour, StopAt: stop},
		{Name: "b", Duration: 4 * time.Hour, StopAt: stop},
		{Name: "c"},
	})
	if len(lines) != 2 || !strings.Contains(lines[0], "a, b: auto-stop in 4h") || !strings.Contains(lines[0], "<!date^") ||
		!strings.Contains(lines[1], "c: no auto-stop") {
		t.Errorf("lines = %v", lines)
	}
}

func TestAppliedStopLines(t *testing.T) {
	t.Parallel()

	stop := time.Date(2026, 11, 8, 14, 0, 0, 0, time.UTC)
	lines := markup.AppliedStopLines([]frontend.Applied{
		{Name: "a", Found: true, StopAt: stop},
		{Name: "b", Found: true, StopAt: stop.Add(300 * time.Millisecond)},
		{Name: "gone", Found: false, StopAt: stop},
		{Name: "c", Found: true},
	})
	if len(lines) != 1 || !strings.Contains(lines[0], "a, b: auto-stop at") {
		t.Errorf("lines = %v", lines)
	}
}

func TestPreviewMetadata_DurationRoundTrip(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(previewMetadata{Cluster: "stg", DurationSec: 5400})
	if err != nil {
		t.Fatal(err)
	}
	var got previewMetadata
	if err := json.Unmarshal(raw, &got); err != nil || got.DurationSec != 5400 {
		t.Errorf("got %+v, %v", got, err)
	}
	raw, _ = json.Marshal(previewMetadata{Cluster: "stg"})
	if strings.Contains(string(raw), "duration_sec") {
		t.Errorf("an unset duration must be omitted: %s", raw)
	}
}
