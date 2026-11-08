package frontend

import (
	"errors"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// Schedule is the resolved auto-stop of one workload start.
type Schedule struct {
	Name string
	// Default and Max are the effective limits of the workload (0 = none).
	Default, Max time.Duration
	// Duration is the resolved run duration; 0 means no auto-stop.
	Duration time.Duration
	// StopAt is now plus Duration; zero when there is no auto-stop.
	StopAt time.Time
	// Err is set when the requested duration is out of range.
	Err error
}

// Schedules resolves the requested durations (by workload name; absent or 0 = default) of names.
func (s *Service) Schedules(env, namespace string, names []string, requested map[string]time.Duration) []Schedule {
	out := make([]Schedule, 0, len(names))
	for _, n := range names {
		out = append(out, s.schedule(env, namespace, n, requested[n]))
	}
	return out
}

func (s *Service) schedule(env, namespace, name string, requested time.Duration) Schedule {
	lim := s.limits(env, namespace, name)
	sch := Schedule{Name: name, Default: lim.Default, Max: lim.Max}
	d, err := lim.Resolve(requested)
	if err != nil {
		sch.Err = err
		return sch
	}
	sch.Duration = d
	if d > 0 {
		sch.StopAt = s.now().Add(d)
	}
	return sch
}

func (s *Service) limits(env, namespace, name string) runlimit.Limits {
	for _, w := range s.store.WorkloadsByNamespace(env, namespace) {
		if w.Name == name {
			return s.runDefaults.For(w)
		}
	}
	return s.runDefaults.For(nil)
}

// Uniform requests the same duration for every name.
func Uniform(names []string, d time.Duration) map[string]time.Duration {
	if d == 0 {
		return nil
	}
	m := make(map[string]time.Duration, len(names))
	for _, n := range names {
		m[n] = d
	}
	return m
}

// ExtendRequest moves the auto-stop of running workloads of one namespace later.
type ExtendRequest struct {
	Cluster   config.Cluster
	Namespace string
	Subject   authz.Subject
	// RequestID ties the audit events of one operation; generated when empty.
	RequestID string
	Names     []string
	// Duration is added to the current stop time.
	Duration time.Duration
}

// Extended is the outcome of extending one workload.
type Extended struct {
	Name string
	// StopAt is the new stop time on success.
	StopAt time.Time
	// Err explains a failure (unknown workload, no auto-stop scheduled, cap exceeded).
	Err error
}

// ExtendResult is the data a frontend renders after extending.
type ExtendResult struct {
	Extended []Extended
	Rejected []Rejection
}

// Extend adds req.Duration to the stop time of each workload, capped by the workload's maximum.
// It is authorized and mode-checked like a start.
func (s *Service) Extend(req ExtendRequest) ExtendResult {
	var res ExtendResult
	if req.RequestID == "" {
		req.RequestID = audit.NewRequestID()
	}
	cl := req.Cluster
	for _, name := range req.Names {
		if reason := s.check(req.Subject, cl.Env, req.Namespace, ActionExtend, name); reason != "" {
			res.Rejected = append(res.Rejected, Rejection{Action: ActionExtend, Label: name, Names: []string{name}, Reason: reason})
			s.record(req.Subject, cl, req.Namespace, name, ActionExtend, audit.Denied, req.RequestID, nil)
			continue
		}
		next, err := s.extendOne(cl.Env, req.Namespace, name, req.Duration)
		res.Extended = append(res.Extended, Extended{Name: name, StopAt: next, Err: err})
		outcome := audit.Success
		if err != nil {
			outcome = audit.Failure
		}
		s.record(req.Subject, cl, req.Namespace, name, ActionExtend, outcome, req.RequestID, err, func(r *audit.Record) {
			r.Duration, r.StopAt = req.Duration, next
		})
	}
	return res
}

func (s *Service) extendOne(env, namespace, name string, add time.Duration) (time.Time, error) {
	var w *model.Workload
	for _, c := range s.store.WorkloadsByNamespace(env, namespace) {
		if c.Name == name {
			w = c
			break
		}
	}
	if w == nil {
		return time.Time{}, errNotFound
	}
	if w.Replicas == 0 {
		return time.Time{}, errors.New("workload is not running")
	}
	next, err := s.runDefaults.For(w).Extend(s.now(), w.StopAt, add)
	if err != nil {
		return time.Time{}, err
	}
	if !s.store.SetStopAt(env, namespace, name, next) {
		return time.Time{}, errNotFound
	}
	return next, nil
}
