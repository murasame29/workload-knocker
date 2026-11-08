package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/model"
)

// WorkloadsHandler handles GET /api/v1/workloads: the workloads the api subject may start or stop,
// read from the in-memory store (no cluster call).
type WorkloadsHandler struct {
	store model.WorkloadStore
	authz authz.Authorizer
}

// NewWorkloadsHandler constructs a WorkloadsHandler.
func NewWorkloadsHandler(store model.WorkloadStore, az authz.Authorizer) *WorkloadsHandler {
	return &WorkloadsHandler{store: store, authz: az}
}

type workloadJSON struct {
	Env       string `json:"env"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	// State is "running" or "stopped".
	State     string `json:"state"`
	StartedBy string `json:"startedBy"`
	// StartedAt / StopAt are null when unknown or when no auto-stop is scheduled.
	StartedAt *time.Time `json:"startedAt"`
	StopAt    *time.Time `json:"stopAt"`
}

type workloadsResponse struct {
	Workloads []workloadJSON `json:"workloads"`
}

// ServeHTTP implements net/http.Handler. Only GET is allowed (405 otherwise).
func (h *WorkloadsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := workloadsResponse{Workloads: h.list()}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "failed to encode response", slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
	}
}

// list returns the visible workloads sorted by env, namespace and name.
func (h *WorkloadsHandler) list() []workloadJSON {
	out := []workloadJSON{}
	for _, c := range h.store.Clusters() {
		for _, ns := range h.store.Namespaces(c.Env) {
			for _, wl := range h.store.WorkloadsByNamespace(c.Env, ns) {
				if !h.visible(c.Env, ns, wl.Name) {
					continue
				}
				out = append(out, toWorkloadJSON(c.Env, ns, wl))
			}
		}
	}
	slices.SortFunc(out, func(a, b workloadJSON) int {
		return cmp.Or(cmp.Compare(a.Env, b.Env), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out
}

func (h *WorkloadsHandler) visible(env, ns, name string) bool {
	return h.authz.Allowed(authz.APISubject, env, ns, name, authz.ActionStart) ||
		h.authz.Allowed(authz.APISubject, env, ns, name, authz.ActionStop)
}

func toWorkloadJSON(env, ns string, wl *model.Workload) workloadJSON {
	j := workloadJSON{Env: env, Namespace: ns, Name: wl.Name, Kind: wl.Kind, State: "stopped"}
	if wl.Replicas > 0 {
		j.State, j.StartedBy = "running", wl.StartedBy
		j.StartedAt, j.StopAt = timePtr(wl.StartedAt), timePtr(wl.StopAt)
	}
	return j
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
