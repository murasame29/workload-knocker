package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

// fakeMetrics records ObserveOperation calls as "env/cluster/operation/result".
type fakeMetrics struct {
	metrics.MetricsManager
	ops []string
}

func (f *fakeMetrics) ObserveOperation(env, cluster, operation, result string, _ time.Duration) {
	f.ops = append(f.ops, env+"/"+cluster+"/"+operation+"/"+result)
}

// fakeStore is a minimal fake of model.WorkloadStore with preset StopAll results.
type fakeStore struct {
	stopAllResult model.StopAllResult
	stopAllCalls  int
}

func (f *fakeStore) Clusters() []model.ClusterRef                          { return nil }
func (f *fakeStore) Namespaces(string) []string                            { return nil }
func (f *fakeStore) WorkloadsByNamespace(string, string) []*model.Workload { return nil }
func (f *fakeStore) Start(string, string, string, string) bool             { return false }
func (f *fakeStore) Stop(string, string, string) bool                      { return false }
func (f *fakeStore) Reconcile(context.Context, string) error               { return nil }
func (f *fakeStore) ReconcileAll(context.Context) error                    { return nil }
func (f *fakeStore) StopAll(_ context.Context) model.StopAllResult {
	f.stopAllCalls++
	return f.stopAllResult
}

func TestStopAllHandler_PostSuccess(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		stopAllResult: model.StopAllResult{
			TotalStopped: 5,
			PerCluster: map[string]model.ClusterStopResult{
				"stg":  {Stopped: 3},
				"prof": {Stopped: 2},
			},
		},
	}

	h := New(store, &fakeMetrics{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stop-all", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}

	var resp stopAllResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.TotalStopped != 5 {
		t.Errorf("TotalStopped: got %d, want 5", resp.TotalStopped)
	}
	if resp.ClustersSucceeded != 2 || resp.ClustersFailed != 0 || resp.ClustersTotal != 2 {
		t.Errorf("clusters counts: got total=%d succ=%d fail=%d, want 2/2/0",
			resp.ClustersTotal, resp.ClustersSucceeded, resp.ClustersFailed)
	}
	if resp.PerCluster["stg"].Stopped != 3 || resp.PerCluster["prof"].Stopped != 2 {
		t.Errorf("per_cluster: got %+v, want stg=3 prof=2", resp.PerCluster)
	}
	if store.stopAllCalls != 1 {
		t.Errorf("StopAll called %d times, want 1", store.stopAllCalls)
	}
}

func TestStopAllHandler_PartialFailure(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		stopAllResult: model.StopAllResult{
			TotalStopped: 3,
			PerCluster: map[string]model.ClusterStopResult{
				"stg":    {Stopped: 3},
				"broken": {Stopped: 0, Error: "simulated failure"},
			},
		},
	}

	h := New(store, &fakeMetrics{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stop-all", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 200 OK even on partial success
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200 (partial success allowed)", rec.Code)
	}

	var resp stopAllResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ClustersSucceeded != 1 || resp.ClustersFailed != 1 {
		t.Errorf("succ=%d fail=%d, want 1/1", resp.ClustersSucceeded, resp.ClustersFailed)
	}
	if resp.PerCluster["broken"].Error != "simulated failure" {
		t.Errorf("broken cluster error: got %q, want %q", resp.PerCluster["broken"].Error, "simulated failure")
	}
}

func TestStopAllHandler_GetReturns405(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	h := New(store, &fakeMetrics{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stop-all", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status: got %d, want 405", rec.Code)
	}
	if store.stopAllCalls != 0 {
		t.Errorf("StopAll should not be called for GET, got %d calls", store.stopAllCalls)
	}
}

func TestStopAllHandler_ObservesPerClusterResult(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		stopAllResult: model.StopAllResult{
			TotalStopped: 3,
			PerCluster: map[string]model.ClusterStopResult{
				"stg":  {Stopped: 3},
				"prof": {Error: "boom"},
			},
		},
	}
	m := &fakeMetrics{}
	h := New(store, m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/stop-all", nil))

	got := map[string]bool{}
	for _, op := range m.ops {
		got[op] = true
	}
	for _, want := range []string{"stg//stop_all/success", "prof//stop_all/error"} {
		if !got[want] {
			t.Errorf("missing observation %q in %v", want, m.ops)
		}
	}
}
