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
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

// StopAllHandler handles /api/v1/stop-all.
// It fans out StopAll to every registered cluster and returns a per-cluster breakdown.
type StopAllHandler struct {
	store   model.WorkloadStore
	metrics metrics.MetricsManager
	authz   authz.Authorizer
	audit   audit.Recorder
}

// New constructs a StopAllHandler; az decides which workloads the api subject may stop.
func New(store model.WorkloadStore, m metrics.MetricsManager, az authz.Authorizer) *StopAllHandler {
	return &StopAllHandler{store: store, metrics: m, authz: az, audit: audit.NewSlog()}
}

// stopAllResponse is the JSON schema of the HTTP response.
type stopAllResponse struct {
	TotalStopped      int                                 `json:"total_stopped"`
	TotalSkipped      int                                 `json:"total_skipped"`
	ClustersTotal     int                                 `json:"clusters_total"`
	ClustersSucceeded int                                 `json:"clusters_succeeded"`
	ClustersFailed    int                                 `json:"clusters_failed"`
	PerCluster        map[string]perClusterStopResultJSON `json:"per_cluster"`
	Message           string                              `json:"message"`
}

// perClusterStopResultJSON is the JSON form of the per-cluster breakdown.
type perClusterStopResultJSON struct {
	Stopped int `json:"stopped"`
	// Skipped lists "namespace/name" of running workloads the authorization policy kept running.
	Skipped []string `json:"skipped"`
	Error   string   `json:"error,omitempty"`
}

// ServeHTTP implements net/http.Handler.
// Stop-All is best-effort, so it returns 200 OK even on partial success;
// clients inspect per_cluster for individual handling.
func (h *StopAllHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	running := h.runningWorkloads()
	begin := time.Now()
	result := h.store.StopAll(r.Context(), func(env, ns, name string) bool {
		return h.authz.Allowed(authz.APISubject, env, ns, name, authz.ActionStop)
	})
	elapsed := time.Since(begin)
	clusterNames := make(map[string]string)
	for _, c := range h.store.Clusters() {
		clusterNames[c.Env] = c.Name
	}

	per := make(map[string]perClusterStopResultJSON, len(result.PerCluster))
	succeeded, failed, skipped := 0, 0, 0
	for k, v := range result.PerCluster {
		skipped += len(v.Skipped)
		per[k] = perClusterStopResultJSON{Stopped: v.Stopped, Skipped: append([]string{}, v.Skipped...), Error: v.Error}
		if v.Error == "" {
			succeeded++
			h.metrics.ObserveOperation(k, clusterNames[k], "stop_all", "success", elapsed)
		} else {
			failed++
			h.metrics.ObserveOperation(k, clusterNames[k], "stop_all", "error", elapsed)
		}
	}

	h.recordAll(r, running, result, clusterNames)

	message := "stop-all dispatched to all clusters"
	if skipped > 0 {
		message = fmt.Sprintf("stop-all dispatched; %d running workloads were skipped by the authorization policy", skipped)
	}

	resp := stopAllResponse{
		TotalStopped:      result.TotalStopped,
		TotalSkipped:      skipped,
		ClustersTotal:     succeeded + failed,
		ClustersSucceeded: succeeded,
		ClustersFailed:    failed,
		PerCluster:        per,
		Message:           message,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "failed to encode response", slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
	}
}

// runningWorkloads snapshots the running workloads before StopAll; the store reports only counts.
func (h *StopAllHandler) runningWorkloads() []model.Workload {
	var out []model.Workload
	for _, c := range h.store.Clusters() {
		for _, ns := range h.store.Namespaces(c.Env) {
			for _, w := range h.store.WorkloadsByNamespace(c.Env, ns) {
				if w.Replicas > 0 {
					out = append(out, model.Workload{Env: c.Env, Namespace: ns, Name: w.Name, Kind: w.Kind})
				}
			}
		}
	}
	return out
}

// recordAll emits one event per workload that was running when the request arrived.
func (h *StopAllHandler) recordAll(r *http.Request, running []model.Workload, result model.StopAllResult, clusterNames map[string]string) {
	requestID, source := audit.NewRequestID(), sourceOf(r)
	for _, w := range running {
		rec := audit.Record{
			Event: audit.WorkloadStopAll, Source: source, RequestID: requestID, EndUser: audit.EndUserAPI,
			Cluster: clusterNames[w.Env], Namespace: w.Namespace, WorkloadName: w.Name, WorkloadKind: w.Kind,
		}
		switch cr := result.PerCluster[w.Env]; {
		case !h.authz.Allowed(authz.APISubject, w.Env, w.Namespace, w.Name, authz.ActionStop):
			rec.Outcome = audit.Denied
		case cr.Error != "":
			rec.Outcome, rec.Err = audit.Failure, errors.New(cr.Error)
		default:
			rec.Outcome = audit.Success
		}
		h.audit.Record(r.Context(), rec)
	}
}
