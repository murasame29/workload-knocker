package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/murasame29/workload-knocker/internal/model"
	nrx "github.com/murasame29/workload-knocker/pkg/newrelic"
)

// StopAllHandler は /api/v1/stop-all エンドポイントのハンドラ。
// 登録された全 cluster の StopAll を fan-out し、per-cluster ブレイクダウンを返す。
type StopAllHandler struct {
	store  model.WorkloadStore
	tracer nrx.Tracer
}

// New は StopAllHandler のコンストラクタ。
func New(store model.WorkloadStore, tracer nrx.Tracer) *StopAllHandler {
	return &StopAllHandler{store: store, tracer: tracer}
}

// stopAllResponse は HTTP レスポンス JSON のスキーマ。
type stopAllResponse struct {
	TotalStopped      int                                 `json:"total_stopped"`
	ClustersTotal     int                                 `json:"clusters_total"`
	ClustersSucceeded int                                 `json:"clusters_succeeded"`
	ClustersFailed    int                                 `json:"clusters_failed"`
	PerCluster        map[string]perClusterStopResultJSON `json:"per_cluster"`
	Message           string                              `json:"message"`
}

// perClusterStopResultJSON は per-cluster ブレイクダウンの JSON 表現。
type perClusterStopResultJSON struct {
	Stopped int    `json:"stopped"`
	Error   string `json:"error,omitempty"`
}

// ServeHTTP は net/http.Handler インターフェースを実装する。
// Stop-All は best-effort オペレーションのため、部分成功でも 200 OK を返す。
// クライアントは per_cluster 結果を見て個別対応する。
func (h *StopAllHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	result := h.store.StopAll(r.Context())

	per := make(map[string]perClusterStopResultJSON, len(result.PerCluster))
	succeeded, failed := 0, 0
	for k, v := range result.PerCluster {
		per[k] = perClusterStopResultJSON{Stopped: v.Stopped, Error: v.Error}
		if v.Error == "" {
			succeeded++
		} else {
			failed++
		}
	}

	h.tracer.RecordEvent("WorkloadKnocker:WorkloadOperation", map[string]any{
		"action":             "stop_all",
		"stopped":            result.TotalStopped,
		"clusters_total":     succeeded + failed,
		"clusters_succeeded": succeeded,
		"clusters_failed":    failed,
		"service.name":       "workload-knocker",
	})

	slog.InfoContext(r.Context(), "stop-all completed", slog.Int("total_stopped", result.TotalStopped), slog.Int("clusters_total", succeeded+failed), slog.Int("clusters_succeeded", succeeded), slog.Int("clusters_failed", failed))

	resp := stopAllResponse{
		TotalStopped:      result.TotalStopped,
		ClustersTotal:     succeeded + failed,
		ClustersSucceeded: succeeded,
		ClustersFailed:    failed,
		PerCluster:        per,
		Message:           "stop-all dispatched to all clusters",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "failed to encode response", slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
	}
}
