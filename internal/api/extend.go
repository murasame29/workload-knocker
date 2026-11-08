package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// ExtendHandler handles /api/v1/extend: it moves the auto-stop of running workloads later.
// Extending is authorized like a start and is allowed in every operation mode.
type ExtendHandler struct {
	store model.WorkloadStore
	authz authz.Authorizer
	audit audit.Recorder
	defs  runlimit.Defaults
	now   func() time.Time
}

// NewExtendHandler constructs an ExtendHandler; az is checked per workload for the api subject.
func NewExtendHandler(store model.WorkloadStore, az authz.Authorizer, defs runlimit.Defaults) *ExtendHandler {
	return &ExtendHandler{store: store, authz: az, audit: audit.NewSlog(), defs: defs, now: time.Now}
}

type extendRequest struct {
	Env       string   `json:"env"`
	Namespace string   `json:"namespace"`
	Workloads []string `json:"workloads"`
	// Duration is added to the current stop time, as a Go duration ("1h"). Required.
	Duration string `json:"duration"`
}

type extendFailure struct {
	Workload string `json:"workload"`
	Message  string `json:"message"`
}

type extendResponse struct {
	Env       string               `json:"env"`
	Namespace string               `json:"namespace"`
	Extended  map[string]time.Time `json:"extended"`
	NotFound  []string             `json:"not_found"`
	Forbidden []string             `json:"forbidden"`
	// Failed lists workloads that could not be extended (stopped, no auto-stop, cap exceeded).
	Failed  []extendFailure `json:"failed"`
	Message string          `json:"message"`
}

// ServeHTTP implements net/http.Handler.
//
// Status codes:
//   - 405: method other than POST
//   - 400: JSON parse failure, missing field, or an invalid duration
//   - 403: every requested workload is refused by the authorization policy
//   - 404: env not in the Cluster Registry, or none of the workloads found
//   - 409: workloads were found but none could be extended
//   - 200: at least one extended
func (h *ExtendHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req extendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	switch {
	case req.Env == "":
		http.Error(w, "env is required", http.StatusBadRequest)
		return
	case req.Namespace == "":
		http.Error(w, "namespace is required", http.StatusBadRequest)
		return
	case len(req.Workloads) == 0:
		http.Error(w, "workloads must not be empty", http.StatusBadRequest)
		return
	case req.Duration == "":
		http.Error(w, "duration is required", http.StatusBadRequest)
		return
	}
	add, err := runlimit.ParseGo(req.Duration)
	if err != nil {
		http.Error(w, "invalid duration: "+err.Error(), http.StatusBadRequest)
		return
	}
	if add < runlimit.MinDuration {
		http.Error(w, "invalid duration: duration must be at least "+runlimit.FormatHours(runlimit.MinDuration), http.StatusBadRequest)
		return
	}
	cluster, ok := h.clusterName(req.Env)
	if !ok {
		http.Error(w, fmt.Sprintf("cluster env %q not found in registry", req.Env), http.StatusNotFound)
		return
	}

	requestID := audit.NewRequestID()
	record := func(name string, outcome audit.Outcome, err error, stopAt time.Time) {
		h.audit.Record(r.Context(), audit.Record{
			Event: audit.WorkloadExtend, Outcome: outcome, Source: audit.SourceAPI,
			RequestID: requestID, EndUser: audit.EndUserAPI,
			Cluster: cluster, Namespace: req.Namespace, WorkloadName: name,
			Duration: add, StopAt: stopAt, Err: err,
		})
	}
	resp := extendResponse{Env: req.Env, Namespace: req.Namespace, Extended: map[string]time.Time{}, NotFound: []string{}, Forbidden: []string{}, Failed: []extendFailure{}}
	for _, name := range req.Workloads {
		if !h.authz.Allowed(authz.APISubject, req.Env, req.Namespace, name, authz.ActionStart) {
			resp.Forbidden = append(resp.Forbidden, name)
			record(name, audit.Denied, nil, time.Time{})
			continue
		}
		wl := h.workload(req.Env, req.Namespace, name)
		if wl == nil {
			resp.NotFound = append(resp.NotFound, name)
			record(name, audit.Failure, errWorkloadNotFound, time.Time{})
			continue
		}
		next, err := h.extend(req.Env, req.Namespace, wl, add)
		if err != nil {
			resp.Failed = append(resp.Failed, extendFailure{Workload: name, Message: err.Error()})
			record(name, audit.Failure, err, time.Time{})
			continue
		}
		resp.Extended[name] = next
		record(name, audit.Success, nil, next)
	}

	status := http.StatusOK
	resp.Message = "workloads extended"
	switch {
	case len(resp.Extended) > 0:
		if len(resp.NotFound)+len(resp.Forbidden)+len(resp.Failed) > 0 {
			resp.Message = "some workloads were extended; others were not found, not authorized or could not be extended"
		}
	case len(resp.Forbidden) > 0 && len(resp.NotFound) == 0 && len(resp.Failed) == 0:
		status, resp.Message = http.StatusForbidden, "not authorized to extend the requested workloads"
	case len(resp.Failed) > 0:
		status, resp.Message = http.StatusConflict, "no workload could be extended"
	default:
		status, resp.Message = http.StatusNotFound, "no matching workloads were found"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "failed to encode response",
			slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
	}
}

func (h *ExtendHandler) extend(env, ns string, wl *model.Workload, add time.Duration) (time.Time, error) {
	if wl.Replicas == 0 {
		return time.Time{}, errors.New("workload is not running")
	}
	next, err := h.defs.For(wl).Extend(h.now(), wl.StopAt, add)
	if err != nil {
		return time.Time{}, err
	}
	if !h.store.SetStopAt(env, ns, wl.Name, next) {
		return time.Time{}, errWorkloadNotFound
	}
	return next, nil
}

func (h *ExtendHandler) workload(env, ns, name string) *model.Workload {
	for _, w := range h.store.WorkloadsByNamespace(env, ns) {
		if w.Name == name {
			return w
		}
	}
	return nil
}

func (h *ExtendHandler) clusterName(env string) (string, bool) {
	for _, c := range h.store.Clusters() {
		if c.Env == env {
			return c.Name, true
		}
	}
	return "", false
}
