// Package mode defines the controller operation mode (OPERATION_MODE).
package mode

import (
	"fmt"
	"strings"
)

// Mode selects which operations the REST API accepts.
type Mode string

const (
	// Full allows the API to start, stop and stop-all.
	Full Mode = "full"
	// ScheduledStop allows API start only; workloads are stopped by the nightly CronJob.
	ScheduledStop Mode = "scheduled-stop"
	// Off allows API start only; there is no scheduled stop, stopping is manual from chat.
	Off Mode = "off"
)

var all = []Mode{Full, ScheduledStop, Off}

// Parse validates s; an empty string yields Full.
func Parse(s string) (Mode, error) {
	if s == "" {
		return Full, nil
	}
	for _, m := range all {
		if string(m) == s {
			return m, nil
		}
	}
	names := make([]string, len(all))
	for i, m := range all {
		names[i] = string(m)
	}
	return "", fmt.Errorf("invalid OPERATION_MODE %q: allowed values are %s", s, strings.Join(names, ", "))
}

// AllowAPIStart reports whether the API may start workloads.
func (Mode) AllowAPIStart() bool { return true }

// AllowAPIStop reports whether the API may stop workloads (stop and stop-all).
func (m Mode) AllowAPIStop() bool { return m == Full }

// UnmarshalText lets caarlos0/env parse and validate OPERATION_MODE.
func (m *Mode) UnmarshalText(b []byte) error {
	p, err := Parse(string(b))
	if err != nil {
		return err
	}
	*m = p
	return nil
}
