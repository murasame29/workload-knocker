package api

import (
	"github.com/murasame29/workload-knocker/internal/notify"
	"github.com/murasame29/workload-knocker/internal/rollout"
)

// settings are the optional collaborators shared by the handlers.
type settings struct {
	notifier notify.Notifier
	tracker  *rollout.Tracker
}

// Option customizes a handler.
type Option func(*settings)

// WithNotifier posts API starts and stops to the cluster's notify channel; the default posts nothing.
func WithNotifier(n notify.Notifier) Option { return func(s *settings) { s.notifier = n } }

// WithTracker follows started workloads until ready, so the notify channel can announce them.
func WithTracker(t *rollout.Tracker) Option { return func(s *settings) { s.tracker = t } }

func newSettings(opts []Option) settings {
	s := settings{notifier: notify.Nop{}}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// apiActor is the display text of REST API callers in notify messages.
const apiActor = "api"
