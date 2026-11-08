package runlimit

import (
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/model"
)

func ptr(d time.Duration) *time.Duration { return &d }

func TestParseHours(t *testing.T) {
	t.Parallel()

	ok := map[string]time.Duration{
		"":     0,
		"  ":   0,
		"3":    3 * time.Hour,
		"1.5":  90 * time.Minute,
		"0.25": 15 * time.Minute,
	}
	for in, want := range ok {
		if got, err := ParseHours(in); err != nil || got != want {
			t.Errorf("ParseHours(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"abc", "-1", "0", "NaN", "Inf", "1e9", "3h"} {
		if _, err := ParseHours(in); err == nil {
			t.Errorf("ParseHours(%q): want error", in)
		}
	}
}

func TestParseGo(t *testing.T) {
	t.Parallel()

	if got, err := ParseGo("90m"); err != nil || got != 90*time.Minute {
		t.Errorf("ParseGo(90m) = %v, %v", got, err)
	}
	if got, err := ParseGo(""); err != nil || got != 0 {
		t.Errorf("ParseGo(empty) = %v, %v", got, err)
	}
	for _, in := range []string{"3", "x", "-1h", "0s"} {
		if _, err := ParseGo(in); err == nil {
			t.Errorf("ParseGo(%q): want error", in)
		}
	}
}

func TestLimits_Resolve(t *testing.T) {
	t.Parallel()

	l := Limits{Default: 4 * time.Hour, Max: 8 * time.Hour}
	if got, err := l.Resolve(0); err != nil || got != 4*time.Hour {
		t.Errorf("empty = %v, %v; want the default", got, err)
	}
	if got, err := l.Resolve(8 * time.Hour); err != nil || got != 8*time.Hour {
		t.Errorf("max = %v, %v", got, err)
	}
	if _, err := l.Resolve(8*time.Hour + time.Minute); err == nil || !strings.Contains(err.Error(), "at most 8h") {
		t.Errorf("above max: %v", err)
	}
	if _, err := l.Resolve(14 * time.Minute); err == nil || !strings.Contains(err.Error(), "at least 0.25h") {
		t.Errorf("below min: %v", err)
	}
	if got, _ := (Limits{}).Resolve(0); got != 0 {
		t.Errorf("no default = %v, want no auto-stop", got)
	}
}

func TestDefaults_For_AnnotationsOverrideAndClamp(t *testing.T) {
	t.Parallel()

	d := Defaults{Default: time.Hour, Max: 24 * time.Hour}
	if got := d.For(nil); got != (Limits{Default: time.Hour, Max: 24 * time.Hour}) {
		t.Errorf("nil workload = %+v", got)
	}
	w := &model.Workload{DefaultDuration: ptr(4 * time.Hour), MaxDuration: 6 * time.Hour}
	if got := d.For(w); got != (Limits{Default: 4 * time.Hour, Max: 6 * time.Hour}) {
		t.Errorf("annotated = %+v", got)
	}
	off := &model.Workload{DefaultDuration: ptr(0)}
	if got := d.For(off).Default; got != 0 {
		t.Errorf("default-duration 0 must disable the default, got %v", got)
	}
	clamp := &model.Workload{MaxDuration: 30 * time.Minute}
	if got := d.For(clamp).Default; got != 30*time.Minute {
		t.Errorf("default must be clamped to max, got %v", got)
	}
}

func TestLimits_Extend(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	l := Limits{Max: 4 * time.Hour}

	if got, err := l.Extend(now, now.Add(time.Hour), time.Hour); err != nil || !got.Equal(now.Add(2*time.Hour)) {
		t.Errorf("extend = %v, %v", got, err)
	}
	if got, err := l.Extend(now, now.Add(-time.Minute), time.Hour); err != nil || !got.Equal(now.Add(time.Hour)) {
		t.Errorf("an already passed stop time extends from now, got %v, %v", got, err)
	}
	if _, err := l.Extend(now, now.Add(3*time.Hour+30*time.Minute), time.Hour); err == nil {
		t.Error("remaining time above max must be rejected")
	}
	if _, err := l.Extend(now, time.Time{}, time.Hour); err == nil {
		t.Error("no auto-stop must be rejected")
	}
	if _, err := l.Extend(now, now.Add(time.Hour), time.Minute); err == nil {
		t.Error("too short extension must be rejected")
	}
}

func TestFromAnnotations(t *testing.T) {
	t.Parallel()

	def, maxDur, problems := FromAnnotations(map[string]string{AnnotationDefaultDuration: "4h", AnnotationMaxDuration: "12h"})
	if def == nil || *def != 4*time.Hour || maxDur != 12*time.Hour || len(problems) != 0 {
		t.Errorf("valid: %v %v %v", def, maxDur, problems)
	}
	def, _, _ = FromAnnotations(map[string]string{AnnotationDefaultDuration: "0"})
	if def == nil || *def != 0 {
		t.Errorf("0 must explicitly disable the default, got %v", def)
	}
	for _, ann := range []map[string]string{
		{AnnotationDefaultDuration: "soon"},
		{AnnotationDefaultDuration: "1m"},
		{AnnotationMaxDuration: "0"},
		{AnnotationMaxDuration: "5m"},
	} {
		if _, _, problems := FromAnnotations(ann); len(problems) != 1 {
			t.Errorf("%v: problems = %v, want 1", ann, problems)
		}
	}
}

func TestFormatHours(t *testing.T) {
	t.Parallel()

	if got := FormatHours(90 * time.Minute); got != "1.5h" {
		t.Errorf("got %q", got)
	}
	if got := FormatHoursNumber(3 * time.Hour); got != "3" {
		t.Errorf("got %q", got)
	}
}
