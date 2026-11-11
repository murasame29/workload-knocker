package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/rollout"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// defaultStartUser is recorded when the start request omits user.
// It marks REST-originated starts, distinct from chat user starts.
const defaultStartUser = "rest-api"

// StartHandler handles /api/v1/start.
// It sets workloads of the given env / namespace to running (replicas=1),
// calling the same store.Start as the chat start flows.
type StartHandler struct {
	store   frontend.Store
	metrics metrics.MetricsManager
	authz   authz.Authorizer
	audit   audit.Recorder
	defs    runlimit.Defaults
	now     func() time.Time
	opts    settings
}

// NewStartHandler constructs a StartHandler; az is checked per workload for the api subject.
// defs are the controller-wide run duration settings.
func NewStartHandler(store frontend.Store, m metrics.MetricsManager, az authz.Authorizer, defs runlimit.Defaults, opts ...Option) *StartHandler {
	return &StartHandler{store: store, metrics: m, authz: az, audit: audit.NewSlog(), defs: defs, now: time.Now, opts: newSettings(opts)}
}

// startRequest is the JSON schema of the POST body.
type startRequest struct {
	// Env is the Cluster Registry env identifier (e.g. stg, dev). Required.
	Env string `json:"env"`
	// Namespace is the target namespace. Required.
	Namespace string `json:"namespace"`
	// Workloads lists workload names to start. At least one is required.
	Workloads []string `json:"workloads"`
	// User identifies the operator. Defaults to "rest-api".
	User string `json:"user,omitempty"`
	// Duration is the run duration as a Go duration ("3h", "90m"); empty uses the workload default.
	Duration string `json:"duration,omitempty"`
}

// startResponse is the JSON schema of the HTTP response.
type startResponse struct {
	Env          string   `json:"env"`
	Namespace    string   `json:"namespace"`
	TotalStarted int      `json:"total_started"`
	Started      []string `json:"started"`
	NotFound     []string `json:"not_found"`
	// Forbidden lists workloads the authorization policy does not let the API start.
	Forbidden []string `json:"forbidden"`
	// Refused maps workloads the actuator refused to start (for example a GitOps conflict) to the reason.
	Refused map[string]string `json:"refused,omitempty"`
	// StopAt maps each started workload with an auto-stop to its stop time (RFC 3339).
	StopAt  map[string]time.Time `json:"stop_at,omitempty"`
	Message string               `json:"message"`
}

// ServeHTTP implements net/http.Handler.
//
// Status codes:
//   - 405: method other than POST
//   - 400: JSON parse failure, missing env / namespace / workloads, or an invalid duration
//   - 403: every requested workload is refused by the authorization policy
//   - 404: env not in the Cluster Registry, or none of the workloads found
//   - 409: none started and every remaining workload was refused by the actuator (see refused)
//   - 200: at least one started (workloads not found are listed in not_found)
func (h *StartHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	if req.Env == "" {
		http.Error(w, "env is required", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		http.Error(w, "namespace is required", http.StatusBadRequest)
		return
	}
	if len(req.Workloads) == 0 {
		http.Error(w, "workloads must not be empty", http.StatusBadRequest)
		return
	}

	// Check that env is registered in the Cluster Registry.
	// store.Start returns false for an unknown env, so reject it with 404 first to tell it from not_found.
	if !h.clusterExists(req.Env) {
		http.Error(w, fmt.Sprintf("cluster env %q not found in registry", req.Env), http.StatusNotFound)
		return
	}

	user := req.User
	if user == "" {
		user = defaultStartUser
	}

	// Refresh the latest ScaledObject state on demand (same as on a Slack command).
	// On failure, continue with the cached state.
	if err := h.store.Reconcile(r.Context(), req.Env); err != nil {
		slog.WarnContext(r.Context(), "ScaledObject reconcile failed, continuing with cache",
			slog.String("cluster.env", req.Env), slog.String("error.message", err.Error()))
	}

	requested, err := runlimit.ParseGo(req.Duration)
	if err != nil {
		http.Error(w, "invalid duration: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Validate every existing workload first so a bad duration starts nothing.
	for _, name := range req.Workloads {
		if wl := h.workload(req.Env, req.Namespace, name); wl != nil {
			if _, err := h.defs.For(wl).Resolve(requested); err != nil {
				http.Error(w, fmt.Sprintf("invalid duration for %s: %v", name, err), http.StatusBadRequest)
				return
			}
		}
	}

	// The shared service authorizes (before touching the store, so a refusal never reveals
	// whether the workload exists), starts, audits, counts and notifies, as for chat.
	areq := frontend.ApplyRequest{
		Cluster:   config.Cluster{Env: req.Env, Name: h.clusterName(req.Env)},
		Namespace: req.Namespace, User: user, Subject: authz.APISubject, RequestID: audit.NewRequestID(),
		ToStart: req.Workloads, Durations: frontend.Uniform(req.Workloads, requested),
	}
	svc := h.service()
	res := svc.Apply(areq)

	stopAts := map[string]time.Time{}
	started := make([]string, 0, len(req.Workloads))
	notFound := make([]string, 0)
	forbidden := make([]string, 0)
	refused := map[string]string{}
	for _, a := range res.Applied {
		if !a.Found {
			notFound = append(notFound, a.Name)
			continue
		}
		started = append(started, a.Name)
		if !a.StopAt.IsZero() {
			stopAts[a.Name] = a.StopAt
		}
	}
	for _, rej := range res.Rejected {
		if rej.Reason == frontend.ReasonRefused {
			refused[rej.Label] = rej.Detail
			continue
		}
		forbidden = append(forbidden, rej.Label)
	}
	h.follow(svc, areq, res)

	status := http.StatusOK
	message := "workloads started"
	switch {
	case len(started) == 0 && len(refused) > 0 && len(notFound) == 0:
		status = http.StatusConflict
		message = "the requested workloads cannot be started; see refused"
	case len(started) == 0 && len(forbidden) > 0 && len(notFound) == 0:
		status = http.StatusForbidden
		message = "not authorized to start the requested workloads"
	case len(started) == 0:
		// None of the specified workloads were found (wrong namespace / name).
		status = http.StatusNotFound
		message = "no matching workloads were found"
	case len(notFound) > 0 || len(forbidden) > 0 || len(refused) > 0:
		message = "some workloads were started; others were not found, not authorized or refused"
	}

	resp := startResponse{
		Env:          req.Env,
		Namespace:    req.Namespace,
		TotalStarted: len(started),
		Started:      started,
		NotFound:     notFound,
		Forbidden:    forbidden,
		Refused:      refused,
		StopAt:       stopAts,
		Message:      message,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "failed to encode response",
			slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
	}
}

// errWorkloadNotFound is the audited error of an unknown workload.
var errWorkloadNotFound = model.ErrWorkloadNotFound

// service builds the shared operation logic from the handler's current settings (tests replace
// the recorder and the clock after construction).
func (h *StartHandler) service() *frontend.Service {
	return newService(h.store, h.metrics, h.authz, h.defs, h.audit, h.now, h.opts)
}

// follow watches the started workloads until ready when a notify channel announced them, so
// the ready edit of the start message happens.
func (h *StartHandler) follow(svc *frontend.Service, req frontend.ApplyRequest, res frontend.ApplyResult) {
	if h.opts.tracker == nil || !h.opts.notifier.Enabled(req.Cluster.Env) {
		return
	}
	targets := svc.RolloutTargets(req, res)
	if len(targets) == 0 {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in api rollout watch", slog.Any("recover", r))
			}
		}()
		h.opts.tracker.Watch(context.Background(), targets, func(rollout.Snapshot) {})
	}()
}

// workload returns a copy of the store's workload, or nil when unknown.
func (h *StartHandler) workload(env, ns, name string) *model.Workload {
	for _, w := range h.store.WorkloadsByNamespace(env, ns) {
		if w.Name == name {
			return w
		}
	}
	return nil
}

// clusterName returns the cluster name of env, or "" when unknown.
func (h *StartHandler) clusterName(env string) string {
	for _, c := range h.store.Clusters() {
		if c.Env == env {
			return c.Name
		}
	}
	return ""
}

// clusterExists reports whether env is registered in the Cluster Registry.
func (h *StartHandler) clusterExists(env string) bool {
	for _, c := range h.store.Clusters() {
		if c.Env == env {
			return true
		}
	}
	return false
}
