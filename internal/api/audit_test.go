package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

type recorderFake struct {
	mu   sync.Mutex
	recs []audit.Record
}

func (f *recorderFake) Record(_ context.Context, r audit.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
}

// auditStore knows two running workloads and one stopped one.
type auditStore struct {
	startStore
	stopAllRes model.StopAllResult
}

func (s *auditStore) Namespaces(string) []string { return []string{"ns"} }
func (s *auditStore) WorkloadsByNamespace(string, string) []*model.Workload {
	return []*model.Workload{
		{Name: "a", Namespace: "ns", Kind: "Deployment", Replicas: 1},
		{Name: "b", Namespace: "ns", Kind: "Rollout", Replicas: 1},
		{Name: "off", Namespace: "ns", Kind: "Deployment"},
		{Name: "ok", Namespace: "ns", Kind: "StatefulSet"},
	}
}
func (s *auditStore) StopAll(context.Context, model.StopFilter) model.StopAllResult {
	return s.stopAllRes
}

func TestStartHandler_AuditEvents(t *testing.T) {
	t.Parallel()

	rec := &recorderFake{}
	h := NewStartHandler(&auditStore{}, &fakeMetrics{}, apiPolicy(t, startTeamOnly), runlimit.Defaults{})
	h.audit = rec
	postStart(t, h, `{"env":"stg","namespace":"team-a","workloads":["ok","billing","okmissing"],"user":"someone"}`)

	want := []audit.Record{
		{Outcome: audit.Success, WorkloadName: "ok", WorkloadKind: "StatefulSet"},
		{Outcome: audit.Denied, WorkloadName: "billing"},
		{Outcome: audit.Failure, WorkloadName: "okmissing", Err: errWorkloadNotFound},
	}
	if len(rec.recs) != len(want) {
		t.Fatalf("records = %+v", rec.recs)
	}
	for i, w := range want {
		w.Event, w.Source, w.EndUser, w.Cluster, w.Namespace = audit.WorkloadStart, audit.SourceAPI, "api", "stg-cluster", "team-a"
		w.RequestID = rec.recs[0].RequestID
		if rec.recs[i] != w || w.RequestID == "" {
			t.Errorf("record %d:\n got  %+v\n want %+v", i, rec.recs[i], w)
		}
	}
}

func TestStopAllHandler_AuditEventsPerWorkload(t *testing.T) {
	t.Parallel()

	const stopA = `
version: 1
rules:
  - env: "*"
    namespaces: ["ns"]
    workloads: ["a"]
    actions: [stop]
    subjects: {api: true}
`
	tests := []struct {
		name   string
		header string
		res    model.StopAllResult
		want   map[string]audit.Outcome
		source audit.Source
	}{
		{"api success and denied", "", model.StopAllResult{PerCluster: map[string]model.ClusterStopResult{"stg": {Stopped: 1}}},
			map[string]audit.Outcome{"a": audit.Success, "b": audit.Denied}, audit.SourceAPI},
		{"nightly stop cluster failure", "nightly-stop", model.StopAllResult{PerCluster: map[string]model.ClusterStopResult{"stg": {Error: "boom"}}},
			map[string]audit.Outcome{"a": audit.Failure, "b": audit.Denied}, audit.SourceNightlyStop},
		{"unknown header value is api", "scheduler", model.StopAllResult{PerCluster: map[string]model.ClusterStopResult{"stg": {Stopped: 1}}},
			map[string]audit.Outcome{"a": audit.Success, "b": audit.Denied}, audit.SourceAPI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorderFake{}
			h := New(&auditStore{stopAllRes: tt.res}, &fakeMetrics{}, apiPolicy(t, stopA))
			h.audit = rec
			req := httptest.NewRequest(http.MethodPost, "/api/v1/stop-all", nil)
			if tt.header != "" {
				req.Header.Set(SourceHeader, tt.header)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if len(rec.recs) != len(tt.want) {
				t.Fatalf("records = %+v", rec.recs)
			}
			for _, r := range rec.recs {
				if r.Event != audit.WorkloadStopAll || r.Source != tt.source || r.EndUser != "api" || r.Outcome != tt.want[r.WorkloadName] ||
					r.Namespace != "ns" || r.WorkloadKind == "" || r.RequestID != rec.recs[0].RequestID || r.RequestID == "" {
					t.Errorf("unexpected record %+v", r)
				}
			}
		})
	}
}

func TestMiddleware_DeniedAuditEvents(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	tests := []struct {
		name   string
		build  func(audit.Recorder) http.Handler
		path   string
		header string
		authz  string
		code   int
		event  audit.Event
		source audit.Source
	}{
		{"start 401", func(r audit.Recorder) http.Handler { return Authenticate(testToken, r, ok) }, "/api/v1/start", "", "Bearer nope", 401, audit.WorkloadStart, audit.SourceAPI},
		{"stop-all 401 from nightly", func(r audit.Recorder) http.Handler { return Authenticate(testToken, r, ok) }, "/api/v1/stop-all", "nightly-stop", "", 401, audit.WorkloadStopAll, audit.SourceNightlyStop},
		{"stop-all 403 by mode", func(r audit.Recorder) http.Handler { return Authenticate(testToken, r, RequireStop(mode.Off, r, ok)) }, "/api/v1/stop-all", "", "Bearer " + testToken, 403, audit.WorkloadStopAll, audit.SourceAPI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorderFake{}
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader("{}"))
			if tt.header != "" {
				req.Header.Set(SourceHeader, tt.header)
			}
			if tt.authz != "" {
				req.Header.Set("Authorization", tt.authz)
			}
			w := httptest.NewRecorder()
			tt.build(rec).ServeHTTP(w, req)

			if w.Code != tt.code || len(rec.recs) != 1 {
				t.Fatalf("code=%d records=%+v", w.Code, rec.recs)
			}
			got := rec.recs[0]
			want := audit.Record{Event: tt.event, Outcome: audit.Denied, Source: tt.source, RequestID: got.RequestID, EndUser: "api"}
			if got != want || got.RequestID == "" {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestMiddleware_NoEventOnSuccessOrDisabledAPI(t *testing.T) {
	t.Parallel()

	rec := &recorderFake{}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/api/v1/start", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	Authenticate(testToken, rec, ok).ServeHTTP(httptest.NewRecorder(), req)
	Authenticate("", rec, ok).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/start", nil))
	if len(rec.recs) != 0 {
		t.Errorf("records = %+v", rec.recs)
	}
}
