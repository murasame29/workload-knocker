package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/murasame29/workload-knocker/internal/model"
	nrx "github.com/murasame29/workload-knocker/pkg/newrelic"
)

// defaultStartUser は start リクエストで user が未指定のときに記録する既定値。
// Slack 経由の起動 (Slack username) と区別できるよう REST 由来であることを示す。
const defaultStartUser = "rest-api"

// StartHandler は /api/v1/start エンドポイントのハンドラ。
// 指定 env / namespace のワークロードを起動状態 (replicas=1) にする。
// Slack Modal の "起動" フローと同じ store.Start を呼ぶ REST 版。
type StartHandler struct {
	store  model.WorkloadStore
	tracer nrx.Tracer
}

// NewStartHandler は StartHandler のコンストラクタ。
func NewStartHandler(store model.WorkloadStore, tracer nrx.Tracer) *StartHandler {
	return &StartHandler{store: store, tracer: tracer}
}

// startRequest は POST body の JSON スキーマ。
type startRequest struct {
	// Env は Cluster Registry の env 識別子 (例: stg, dev)。必須。
	Env string `json:"env"`
	// Namespace は対象 namespace。必須。
	Namespace string `json:"namespace"`
	// Workloads は起動する workload 名のリスト。1 件以上必須。
	Workloads []string `json:"workloads"`
	// User は操作者の識別子。省略時は "rest-api"。
	User string `json:"user,omitempty"`
}

// startResponse は HTTP レスポンス JSON のスキーマ。
type startResponse struct {
	Env          string   `json:"env"`
	Namespace    string   `json:"namespace"`
	TotalStarted int      `json:"total_started"`
	Started      []string `json:"started"`
	NotFound     []string `json:"not_found"`
	Message      string   `json:"message"`
}

// ServeHTTP は net/http.Handler インターフェースを実装する。
//
// ステータスコード:
//   - 405: POST 以外
//   - 400: JSON パース失敗 / env・namespace・workloads の必須欠落
//   - 404: env が Cluster Registry に存在しない / 指定 workload が 1 件も見つからない
//   - 200: 1 件以上起動できた (見つからなかった workload は not_found に列挙)
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

	// env が Cluster Registry に登録されているか確認する。
	// 未登録の env は store.Start が false を返すが、workload not_found と区別するため先に 404 で弾く。
	if !h.clusterExists(req.Env) {
		http.Error(w, fmt.Sprintf("cluster env %q not found in registry", req.Env), http.StatusNotFound)
		return
	}

	user := req.User
	if user == "" {
		user = defaultStartUser
	}

	// 最新の ScaledObject 状態をオンデマンドで反映する (Slack コマンド受信時と同じ)。
	// 失敗してもキャッシュ済みの状態で続行する。
	if err := h.store.Reconcile(r.Context(), req.Env); err != nil {
		slog.WarnContext(r.Context(), "ScaledObject の reconcile に失敗、キャッシュで続行",
			slog.String("cluster.env", req.Env), slog.String("error.message", err.Error()))
	}

	started := make([]string, 0, len(req.Workloads))
	notFound := make([]string, 0)
	for _, name := range req.Workloads {
		if h.store.Start(req.Env, req.Namespace, name, user) {
			started = append(started, name)
			slog.InfoContext(r.Context(), "workload started via REST API",
				slog.String("cluster.env", req.Env), slog.String("namespace", req.Namespace),
				slog.String("workload", name), slog.String("user.name", user))
			h.tracer.RecordEvent("WorkloadKnocker:WorkloadOperation", map[string]any{
				"action":       "start",
				"env":          req.Env,
				"namespace":    req.Namespace,
				"workload":     name,
				"user":         user,
				"source":       "rest-api",
				"service.name": "workload-knocker",
			})
		} else {
			notFound = append(notFound, name)
		}
	}

	status := http.StatusOK
	message := "workloads started"
	if len(started) == 0 {
		// 指定 workload が 1 件も見つからなかった (namespace / name の指定ミス)。
		status = http.StatusNotFound
		message = "no matching workloads were found"
	} else if len(notFound) > 0 {
		message = "some workloads were started; others were not found"
	}

	slog.InfoContext(r.Context(), "start request completed",
		slog.String("cluster.env", req.Env), slog.String("namespace", req.Namespace),
		slog.Int("total_started", len(started)), slog.Int("not_found", len(notFound)))

	resp := startResponse{
		Env:          req.Env,
		Namespace:    req.Namespace,
		TotalStarted: len(started),
		Started:      started,
		NotFound:     notFound,
		Message:      message,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "failed to encode response",
			slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
	}
}

// clusterExists は env が Cluster Registry に登録されているかを返す。
func (h *StartHandler) clusterExists(env string) bool {
	for _, c := range h.store.Clusters() {
		if c.Env == env {
			return true
		}
	}
	return false
}
