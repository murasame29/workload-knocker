package api

import (
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/notify"
	"github.com/murasame29/workload-knocker/internal/rollout"
	"github.com/murasame29/workload-knocker/internal/runlimit"
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

// newService builds the shared operation logic used by the API handlers; m may be nil (no
// operation metrics, as for extend).
func newService(store model.WorkloadStore, m metrics.MetricsManager, az authz.Authorizer, defs runlimit.Defaults, rec audit.Recorder, now func() time.Time, s settings) *frontend.Service {
	if m == nil {
		m = metrics.New()
	}
	opts := []frontend.Option{frontend.WithAuthorizer(az), frontend.WithRunDefaults(defs), frontend.WithRecorder(rec), frontend.WithClock(now)}
	if s.notifier != nil {
		opts = append(opts, frontend.WithNotifier(s.notifier))
	}
	// Chat stop is irrelevant here: the API starts and extends only.
	return frontend.NewService(store, m, 0, opts...)
}
