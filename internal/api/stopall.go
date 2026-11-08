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

// StopAllHandler handles /api/v1/stop-all.
// It fans out StopAll to every registered cluster and returns a per-cluster breakdown.
type StopAllHandler struct {
	store   model.WorkloadStore
	metrics metrics.MetricsManager
	authz   authz.Authorizer
}

// New constructs a StopAllHandler; az decides which workloads the api subject may stop.
func New(store model.WorkloadStore, m metrics.MetricsManager, az authz.Authorizer) *StopAllHandler {
	return &StopAllHandler{store: store, metrics: m, authz: az}
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

	slog.InfoContext(r.Context(), "stop-all completed", slog.Int("total_stopped", result.TotalStopped), slog.Int("clusters_total", succeeded+failed), slog.Int("clusters_succeeded", succeeded), slog.Int("clusters_failed", failed), slog.Int("total_skipped", skipped))

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
