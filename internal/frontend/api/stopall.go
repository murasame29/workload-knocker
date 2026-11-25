package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/murasame29/workload-knocker/internal/frontend/notify"
	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

// StopAllHandler handles /api/v1/stop-all.
// It fans out StopAll to every registered cluster and returns a per-cluster breakdown.
// With ?skip=reserved, workloads whose current run was started by a reservation keep running.
type StopAllHandler struct {
	store   stopAllStore
	metrics metrics.MetricsManager
	authz   authz.Authorizer
	audit   audit.Recorder
	opts    settings
}

// stopAllStore is the part of the workload store stop-all needs.
type stopAllStore interface {
	model.WorkloadReader
	StopAll(ctx context.Context, allow model.StopFilter) model.StopAllResult
}

// New constructs a StopAllHandler; az decides which workloads the api subject may stop.
func New(store stopAllStore, m metrics.MetricsManager, az authz.Authorizer, opts ...Option) *StopAllHandler {
	return &StopAllHandler{store: store, metrics: m, authz: az, audit: audit.NewSlog(), opts: newSettings(opts)}
}

// stopAllResponse is the JSON schema of the HTTP response.
type stopAllResponse struct {
	TotalStopped int `json:"total_stopped"`
	TotalSkipped int `json:"total_skipped"`
	// TotalReserved counts the reservation runs kept by ?skip=reserved.
	TotalReserved     int                                 `json:"total_reserved"`
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
	// Reserved lists "namespace/name" of reservation runs kept by ?skip=reserved.
	Reserved []string `json:"reserved,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// skipParam is the query parameter of stop-all; skipReserved is its only value.
const (
	skipParam    = "skip"
	skipReserved = "reserved"
)

// parseSkip reads ?skip=reserved; any other value is a client error.
func parseSkip(r *http.Request) (bool, error) {
	switch v := r.URL.Query().Get(skipParam); v {
	case "":
		return false, nil
	case skipReserved:
		return true, nil
	default:
		return false, fmt.Errorf("unknown %s value %q (supported: %s)", skipParam, v, skipReserved)
	}
}

// ServeHTTP implements net/http.Handler.
// Stop-All is best-effort, so it returns 200 OK even on partial success;
// clients inspect per_cluster for individual handling.
func (h *StopAllHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	keepReserved, err := parseSkip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	running := h.runningWorkloads()
	kept := map[string]bool{}
	if keepReserved {
		for _, wl := range running {
			if wl.Reservation != "" {
				kept[wl.Env+"/"+wl.Namespace+"/"+wl.Name] = true
			}
		}
	}
	begin := time.Now()
	result := h.store.StopAll(r.Context(), func(env, ns, name string) bool {
		return !kept[env+"/"+ns+"/"+name] && h.authz.Allowed(authz.APISubject, env, ns, name, authz.ActionStop)
	})
	elapsed := time.Since(begin)
	clusterNames := make(map[string]string)
	for _, c := range h.store.Clusters() {
		clusterNames[c.Env] = c.Name
	}

	per := make(map[string]perClusterStopResultJSON, len(result.PerCluster))
	succeeded, failed, skipped, reserved := 0, 0, 0, 0
	for k, v := range result.PerCluster {
		pc := perClusterStopResultJSON{Stopped: v.Stopped, Skipped: []string{}, Error: v.Error}
		for _, s := range v.Skipped {
			if kept[k+"/"+s] {
				pc.Reserved = append(pc.Reserved, s)
			} else {
				pc.Skipped = append(pc.Skipped, s)
			}
		}
		skipped += len(pc.Skipped)
		reserved += len(pc.Reserved)
		per[k] = pc
		if v.Error == "" {
			succeeded++
			h.metrics.ObserveOperation(k, clusterNames[k], "stop_all", "success", elapsed)
		} else {
			failed++
			h.metrics.ObserveOperation(k, clusterNames[k], "stop_all", "error", elapsed)
		}
	}

	// Reservation runs that were kept were neither stopped nor refused: leave them out of the
	// audit and the notify message.
	stoppable := slices.DeleteFunc(slices.Clone(running), func(wl model.Workload) bool { return kept[wl.Env+"/"+wl.Namespace+"/"+wl.Name] })
	h.recordAll(r, stoppable, result, clusterNames)
	h.announce(r, stoppable, result, clusterNames)

	message := "stop-all dispatched to all clusters"
	if skipped > 0 {
		message = fmt.Sprintf("stop-all dispatched; %d running workloads were skipped by the authorization policy", skipped)
	}
	if reserved > 0 {
		message += fmt.Sprintf("; %d workloads started by a reservation kept running", reserved)
	}

	resp := stopAllResponse{
		TotalStopped:      result.TotalStopped,
		TotalSkipped:      skipped,
		TotalReserved:     reserved,
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
					out = append(out, model.Workload{Env: c.Env, Namespace: ns, Name: w.Name, Kind: w.Kind, Reservation: w.Reservation})
				}
			}
		}
	}
	return out
}

// announce posts the workloads that were stopped to the notify channel of their cluster, one message per cluster.
func (h *StopAllHandler) announce(r *http.Request, running []model.Workload, result model.StopAllResult, clusterNames map[string]string) {
	byEnv := map[string][]notify.Workload{}
	for _, w := range running {
		if result.PerCluster[w.Env].Error != "" || !h.authz.Allowed(authz.APISubject, w.Env, w.Namespace, w.Name, authz.ActionStop) {
			continue
		}
		byEnv[w.Env] = append(byEnv[w.Env], notify.Workload{Namespace: w.Namespace, Name: w.Name})
	}
	actor := string(sourceOf(r))
	if actor == string(audit.SourceAPI) {
		actor = apiActor
	}
	envs := make([]string, 0, len(byEnv))
	for env := range byEnv {
		envs = append(envs, env)
	}
	slices.Sort(envs)
	for _, env := range envs {
		h.opts.notifier.Stopped(r.Context(), notify.Event{
			Kind: notify.KindStopAll, Env: env, ClusterName: clusterNames[env], Actor: actor, Workloads: byEnv[env],
		})
	}
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
