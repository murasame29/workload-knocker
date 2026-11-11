package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/model"
)

// listStore serves the list path; every cluster call and mutation fails the test.
type listStore struct {
	t  *testing.T
	ws map[string][]*model.Workload // key: env/namespace
}

func (s *listStore) Clusters() []model.ClusterRef {
	return []model.ClusterRef{{Env: "stg", Name: "stg-cluster"}, {Env: "dev", Name: "dev-cluster"}}
}

func (s *listStore) Namespaces(env string) []string {
	var out []string
	for k := range s.ws {
		if e, ns, ok := strings.Cut(k, "/"); ok && e == env {
			out = append(out, ns)
		}
	}
	return out
}

func (s *listStore) WorkloadsByNamespace(env, ns string) []*model.Workload { return s.ws[env+"/"+ns] }

func (s *listStore) Reconcile(context.Context, string) error {
	s.t.Error("GET /api/v1/workloads must not call Reconcile")
	return nil
}

func (s *listStore) ReconcileAll(context.Context) error {
	s.t.Error("GET /api/v1/workloads must not call ReconcileAll")
	return nil
}

func (s *listStore) Start(string, string, string, string, time.Time) error {
	s.t.Error("GET /api/v1/workloads must not start")
	return model.ErrWorkloadNotFound
}

func (s *listStore) SetStopAt(string, string, string, time.Time) bool {
	s.t.Error("GET /api/v1/workloads must not change a stop time")
	return false
}

func (s *listStore) Stop(string, string, string) error {
	s.t.Error("GET /api/v1/workloads must not stop")
	return model.ErrWorkloadNotFound
}

func (s *listStore) StopAll(context.Context, model.StopFilter) model.StopAllResult {
	s.t.Error("GET /api/v1/workloads must not stop all")
	return model.StopAllResult{}
}

func getWorkloads(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, workloadsResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil))
	var resp workloadsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
		}
	}
	return rec, resp
}

func TestWorkloadsHandler_ListsStateAndTimes(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	stopAt := startedAt.Add(3 * time.Hour)
	store := &listStore{t: t, ws: map[string][]*model.Workload{
		"stg/labs": {
			{Name: "web", Namespace: "labs", Kind: "Deployment", Replicas: 1, StartedBy: "alice", StartedAt: startedAt, StopAt: stopAt},
			{Name: "batch", Namespace: "labs", Kind: "Rollout", Replicas: 1},
			{Name: "idle", Namespace: "labs", Kind: "Deployment", StartedBy: "stale"},
		},
		"dev/play": {{Name: "game", Namespace: "play", Kind: "StatefulSet"}},
	}}
	rec, resp := getWorkloads(t, NewWorkloadsHandler(store, allowAll{}))

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var names []string
	for _, w := range resp.Workloads {
		names = append(names, w.Env+"/"+w.Namespace+"/"+w.Name)
	}
	if got, want := strings.Join(names, ","), "dev/play/game,stg/labs/batch,stg/labs/idle,stg/labs/web"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	byName := map[string]workloadJSON{}
	for _, w := range resp.Workloads {
		byName[w.Name] = w
	}
	web := byName["web"]
	if web.State != "running" || web.Kind != "Deployment" || web.StartedBy != "alice" ||
		web.StartedAt == nil || !web.StartedAt.Equal(startedAt) || web.StopAt == nil || !web.StopAt.Equal(stopAt) {
		t.Errorf("web = %+v", web)
	}
	if batch := byName["batch"]; batch.State != "running" || batch.StartedAt != nil || batch.StopAt != nil {
		t.Errorf("a running workload without recorded times has null times: %+v", batch)
	}
	if idle := byName["idle"]; idle.State != "stopped" || idle.StartedBy != "" || idle.StopAt != nil {
		t.Errorf("a stopped workload carries no run data: %+v", idle)
	}
}

func TestWorkloadsHandler_JSONShape(t *testing.T) {
	t.Parallel()

	store := &listStore{t: t, ws: map[string][]*model.Workload{"stg/labs": {{Name: "idle", Namespace: "labs", Kind: "Deployment"}}}}
	rec := httptest.NewRecorder()
	NewWorkloadsHandler(store, allowAll{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil))

	want := `{"workloads":[{"env":"stg","namespace":"labs","name":"idle","kind":"Deployment","state":"stopped","startedBy":"","startedAt":null,"stopAt":null}]}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body = %s\nwant   %s", got, want)
	}
}

func TestWorkloadsHandler_EmptyIsAnEmptyArray(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	NewWorkloadsHandler(&listStore{t: t}, allowAll{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"workloads":[]}` {
		t.Errorf("body = %s", got)
	}
}

const listTeamOnly = `
version: 1
rules:
  - env: stg
    namespaces: ["team-*"]
    workloads: ["web*"]
    actions: [stop]
    subjects: {api: true}
`

func TestWorkloadsHandler_FiltersByPolicy(t *testing.T) {
	t.Parallel()

	store := &listStore{t: t, ws: map[string][]*model.Workload{
		"stg/team-a": {{Name: "web", Namespace: "team-a", Replicas: 1}, {Name: "billing", Namespace: "team-a", Replicas: 1}},
		"stg/other":  {{Name: "web", Namespace: "other", Replicas: 1}},
		"dev/team-a": {{Name: "web", Namespace: "team-a", Replicas: 1}},
	}}
	_, resp := getWorkloads(t, NewWorkloadsHandler(store, apiPolicy(t, listTeamOnly)))

	if len(resp.Workloads) != 1 || resp.Workloads[0].Env != "stg" || resp.Workloads[0].Namespace != "team-a" || resp.Workloads[0].Name != "web" {
		t.Errorf("workloads = %+v, want only stg/team-a/web (stop permission alone makes it visible)", resp.Workloads)
	}
}

func TestWorkloadsHandler_OnlyGET(t *testing.T) {
	t.Parallel()

	h := NewWorkloadsHandler(&listStore{t: t}, allowAll{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/workloads", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: status=%d allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

func TestWorkloadsHandler_RequiresBearerToken(t *testing.T) {
	t.Parallel()

	h := Authenticate(testToken, audit.NewSlog(), NewWorkloadsHandler(&listStore{t: t}, allowAll{}))
	for name, authz := range map[string]string{"missing": "", "wrong": "Bearer nope"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s token: status = %d, want 401", name, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid token: status = %d, want 200", rec.Code)
	}
}

type captureRecorder struct{ records []audit.Record }

func (c *captureRecorder) Record(_ context.Context, r audit.Record) { c.records = append(c.records, r) }

// A refused list request is not a workload operation, so it emits no audit event.
func TestWorkloadsHandler_RefusedRequestIsNotAudited(t *testing.T) {
	t.Parallel()

	rec := &captureRecorder{}
	h := Authenticate(testToken, rec, NewWorkloadsHandler(&listStore{t: t}, allowAll{}))
	out := httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/v1/workloads", nil))
	if out.Code != http.StatusUnauthorized || len(rec.records) != 0 {
		t.Errorf("status=%d audit events=%d, want 401 and none", out.Code, len(rec.records))
	}
}
