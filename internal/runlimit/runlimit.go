// Package runlimit resolves the run duration of a start and the cap on extending it.
// The behavior is documented in docs/en/configuration.md (Run duration).
package runlimit

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/murasame29/workload-knocker/internal/model"
)

// ScaledObject annotations that override the controller-wide settings per workload.
const (
	AnnotationDefaultDuration = "workload-knocker.io/default-duration"
	AnnotationMaxDuration     = "workload-knocker.io/max-duration"
)

// MinDuration is the shortest run duration a start or an extension may request.
const MinDuration = 15 * time.Minute

// Defaults are the controller-wide settings (DEFAULT_RUN_DURATION / MAX_RUN_DURATION).
// Default 0 means no auto-stop; Max 0 means no cap.
type Defaults struct {
	Default time.Duration
	Max     time.Duration
}

// Limits are the effective settings of one workload.
type Limits struct {
	Default time.Duration
	Max     time.Duration
}

// For merges the workload annotations over the controller-wide defaults.
func (d Defaults) For(w *model.Workload) Limits {
	l := Limits(d)
	if w == nil {
		return l
	}
	if w.DefaultDuration != nil {
		l.Default = *w.DefaultDuration
	}
	if w.MaxDuration > 0 {
		l.Max = w.MaxDuration
	}
	if l.Max > 0 && l.Default > l.Max {
		l.Default = l.Max
	}
	return l
}

// Resolve returns the run duration for a requested one; 0 selects the default (0 = no auto-stop).
func (l Limits) Resolve(requested time.Duration) (time.Duration, error) {
	if requested == 0 {
		return l.Default, nil
	}
	if err := l.check(requested); err != nil {
		return 0, err
	}
	return requested, nil
}

func (l Limits) check(d time.Duration) error {
	if d < MinDuration {
		return fmt.Errorf("duration must be at least %s", FormatHours(MinDuration))
	}
	if l.Max > 0 && d > l.Max {
		return fmt.Errorf("duration must be at most %s", FormatHours(l.Max))
	}
	return nil
}

// Extend returns the new stop time after adding add to the current one (or to now when it has
// already passed). The remaining run time after extending may not exceed Max.
func (l Limits) Extend(now, stopAt time.Time, add time.Duration) (time.Time, error) {
	if stopAt.IsZero() {
		return time.Time{}, fmt.Errorf("no auto-stop is scheduled")
	}
	if add < MinDuration {
		return time.Time{}, fmt.Errorf("duration must be at least %s", FormatHours(MinDuration))
	}
	base := stopAt
	if base.Before(now) {
		base = now
	}
	next := base.Add(add)
	if l.Max > 0 && next.Sub(now) > l.Max {
		return time.Time{}, fmt.Errorf("the remaining run time may not exceed %s", FormatHours(l.Max))
	}
	return next, nil
}

// ParseHours parses a number of hours such as "3" or "1.5". An empty string yields 0 (default).
func ParseHours(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	h, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(h) || math.IsInf(h, 0) {
		return 0, fmt.Errorf("%q is not a number of hours (for example 3 or 1.5)", s)
	}
	if h > 24*365 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	d := time.Duration(math.Round(h * float64(time.Hour)))
	if d < MinDuration {
		return 0, fmt.Errorf("duration must be at least %s", FormatHours(MinDuration))
	}
	return d, nil
}

// ParseGo parses a Go duration such as "3h" or "90m". An empty string yields 0 (default).
func ParseGo(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (for example 3h or 90m)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be at least %s", FormatHours(MinDuration))
	}
	return d, nil
}

// FormatHours renders d as hours with the shortest exact decimal, for example "3h" or "1.5h".
func FormatHours(d time.Duration) string {
	return strconv.FormatFloat(d.Hours(), 'f', -1, 64) + "h"
}

// FormatHoursNumber renders d as a bare number of hours, for example "1.5".
func FormatHoursNumber(d time.Duration) string {
	return strconv.FormatFloat(d.Hours(), 'f', -1, 64)
}

// FromAnnotations reads the per-workload overrides. Invalid values are reported and ignored.
func FromAnnotations(ann map[string]string) (def *time.Duration, maxDur time.Duration, problems []string) {
	if v, ok := ann[AnnotationDefaultDuration]; ok {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil || d < 0 || (d > 0 && d < MinDuration) {
			problems = append(problems, AnnotationDefaultDuration)
		} else {
			def = &d
		}
	}
	if v, ok := ann[AnnotationMaxDuration]; ok {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil || d < MinDuration {
			problems = append(problems, AnnotationMaxDuration)
		} else {
			maxDur = d
		}
	}
	return def, maxDur, problems
}
