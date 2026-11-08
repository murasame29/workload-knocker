package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

// defaultStartUser is recorded when the start request omits user.
// It marks REST-originated starts, distinct from chat user starts.
const defaultStartUser = "rest-api"

// StartHandler handles /api/v1/start.
// It sets workloads of the given env / namespace to running (replicas=1),
// calling the same store.Start as the chat start flows.
type StartHandler struct {
	store   model.WorkloadStore
	metrics metrics.MetricsManager
	authz   authz.Authorizer
}

// NewStartHandler constructs a StartHandler; az is checked per workload for the api subject.
func NewStartHandler(store model.WorkloadStore, m metrics.MetricsManager, az authz.Authorizer) *StartHandler {
	return &StartHandler{store: store, metrics: m, authz: az}
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
	Message   string   `json:"message"`
}

// ServeHTTP implements net/http.Handler.
//
// Status codes:
//   - 405: method other than POST
//   - 400: JSON parse failure, or missing env / namespace / workloads
//   - 403: every requested workload is refused by the authorization policy
//   - 404: env not in the Cluster Registry, or none of the workloads found
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

	cluster := h.clusterName(req.Env)
	started := make([]string, 0, len(req.Workloads))
	notFound := make([]string, 0)
	forbidden := make([]string, 0)
	for _, name := range req.Workloads {
		// Authorize before touching the store so a refusal never reveals whether the workload exists.
		if !h.authz.Allowed(authz.APISubject, req.Env, req.Namespace, name, authz.ActionStart) {
			forbidden = append(forbidden, name)
			slog.WarnContext(r.Context(), "start refused by authorization policy",
				slog.String("cluster.env", req.Env), slog.String("namespace", req.Namespace), slog.String("workload", name))
			continue
		}
		begin := time.Now()
		ok := h.store.Start(req.Env, req.Namespace, name, user)
		if ok {
			h.metrics.ObserveOperation(req.Env, cluster, "start", "success", time.Since(begin))
			started = append(started, name)
			slog.InfoContext(r.Context(), "workload started via REST API",
				slog.String("cluster.env", req.Env), slog.String("namespace", req.Namespace),
				slog.String("workload", name), slog.String("user.name", user))
		} else {
			h.metrics.ObserveOperation(req.Env, cluster, "start", "not_found", time.Since(begin))
			notFound = append(notFound, name)
		}
	}

	status := http.StatusOK
	message := "workloads started"
	switch {
	case len(started) == 0 && len(forbidden) > 0 && len(notFound) == 0:
		status = http.StatusForbidden
		message = "not authorized to start the requested workloads"
	case len(started) == 0:
		// None of the specified workloads were found (wrong namespace / name).
		status = http.StatusNotFound
		message = "no matching workloads were found"
	case len(notFound) > 0 || len(forbidden) > 0:
		message = "some workloads were started; others were not found or not authorized"
	}

	slog.InfoContext(r.Context(), "start request completed",
		slog.String("cluster.env", req.Env), slog.String("namespace", req.Namespace),
		slog.Int("total_started", len(started)), slog.Int("not_found", len(notFound)), slog.Int("forbidden", len(forbidden)))

	resp := startResponse{
		Env:          req.Env,
		Namespace:    req.Namespace,
		TotalStarted: len(started),
		Started:      started,
		NotFound:     notFound,
		Forbidden:    forbidden,
		Message:      message,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "failed to encode response",
			slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
	}
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
