package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/murasame29/workload-knocker/internal/workload/model"
)

// filterStore runs the StopAll filter over its running workloads like the real store.
type filterStore struct {
	fakeStore
	workloads []*model.Workload
	stopped   []string
}

func (s *filterStore) Clusters() []model.ClusterRef {
	return []model.ClusterRef{{Env: "stg", Name: "c"}}
}
func (s *filterStore) Namespaces(string) []string { return []string{"labs"} }
func (s *filterStore) WorkloadsByNamespace(string, string) []*model.Workload {
	return s.workloads
}

func (s *filterStore) StopAll(_ context.Context, allow model.StopFilter) model.StopAllResult {
	res := model.ClusterStopResult{}
	for _, w := range s.workloads {
		if w.Replicas == 0 {
			continue
		}
		if !allow("stg", w.Namespace, w.Name) {
			res.Skipped = append(res.Skipped, w.Namespace+"/"+w.Name)
			continue
		}
		res.Stopped++
		s.stopped = append(s.stopped, w.Name)
	}
	return model.StopAllResult{TotalStopped: res.Stopped, PerCluster: map[string]model.ClusterStopResult{"stg": res}}
}

func reservedWorkloads() []*model.Workload {
	return []*model.Workload{
		{Env: "stg", Namespace: "labs", Name: "manual", Replicas: 1, StartedBy: "alice"},
		{Env: "stg", Namespace: "labs", Name: "booked", Replicas: 1, StartedBy: "alice", Reservation: "r1"},
	}
}

func TestStopAll_SkipReservedKeepsReservationRuns(t *testing.T) {
	t.Parallel()
	store := &filterStore{workloads: reservedWorkloads()}
	h := New(store, &fakeMetrics{}, allowAll{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/stop-all?skip=reserved", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !slices.Equal(store.stopped, []string{"manual"}) {
		t.Errorf("stopped = %v, want only the manual run", store.stopped)
	}
	var resp stopAllResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	pc := resp.PerCluster["stg"]
	if resp.TotalReserved != 1 || resp.TotalSkipped != 0 || !slices.Equal(pc.Reserved, []string{"labs/booked"}) || len(pc.Skipped) != 0 {
		t.Errorf("response = %+v", resp)
	}
}

func TestStopAll_WithoutSkipStopsEverything(t *testing.T) {
	t.Parallel()
	store := &filterStore{workloads: reservedWorkloads()}
	h := New(store, &fakeMetrics{}, allowAll{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/stop-all", nil))

	if rec.Code != http.StatusOK || len(store.stopped) != 2 {
		t.Errorf("status %d, stopped %v, want both", rec.Code, store.stopped)
	}
}

func TestStopAll_UnknownSkipIsABadRequest(t *testing.T) {
	t.Parallel()
	store := &filterStore{workloads: reservedWorkloads()}
	h := New(store, &fakeMetrics{}, allowAll{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/stop-all?skip=everything", nil))

	if rec.Code != http.StatusBadRequest || len(store.stopped) != 0 {
		t.Errorf("status %d, stopped %v, want 400 and nothing stopped", rec.Code, store.stopped)
	}
}
