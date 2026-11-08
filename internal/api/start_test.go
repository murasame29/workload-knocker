package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// startStore starts only workloads named "ok".
type startStore struct {
	fakeStore
	started []string
}

func (s *startStore) Clusters() []model.ClusterRef {
	return []model.ClusterRef{{Env: "stg", Name: "stg-cluster"}}
}

func (s *startStore) Start(_, _, name, _ string, _ time.Time) bool {
	s.started = append(s.started, name)
	return name == "ok" || name == "ok2"
}

func TestStartHandler_ObservesSuccessAndNotFound(t *testing.T) {
	t.Parallel()

	m := &fakeMetrics{}
	h := NewStartHandler(&startStore{}, m, allowAll{}, runlimit.Defaults{})
	body := `{"env":"stg","namespace":"ns","workloads":["ok","missing"]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/start", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	want := []string{"stg/stg-cluster/start/success", "stg/stg-cluster/start/not_found"}
	if len(m.ops) != len(want) || m.ops[0] != want[0] || m.ops[1] != want[1] {
		t.Fatalf("observations: got %v, want %v", m.ops, want)
	}
}

const startTeamOnly = `
version: 1
rules:
  - env: stg
    namespaces: ["team-*"]
    workloads: ["ok*"]
    actions: [start]
    subjects: {api: true}
`

func postStart(t *testing.T, h *StartHandler, body string) (*httptest.ResponseRecorder, startResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/start", strings.NewReader(body)))
	var resp startResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return rec, resp
}

func TestStartHandler_RefusesWorkloadsOutsidePolicy(t *testing.T) {
	t.Parallel()

	store := &startStore{}
	h := NewStartHandler(store, &fakeMetrics{}, apiPolicy(t, startTeamOnly), runlimit.Defaults{})

	rec, resp := postStart(t, h, `{"env":"stg","namespace":"team-a","workloads":["ok","billing"]}`)
	if rec.Code != http.StatusOK || !slices.Equal(resp.Started, []string{"ok"}) || !slices.Equal(resp.Forbidden, []string{"billing"}) {
		t.Errorf("status=%d resp=%+v", rec.Code, resp)
	}
	if slices.Contains(store.started, "billing") {
		t.Errorf("store.Start called for a forbidden workload: %v", store.started)
	}
}

func TestStartHandler_AllForbiddenIs403WithoutTouchingStore(t *testing.T) {
	t.Parallel()

	store := &startStore{}
	h := NewStartHandler(store, &fakeMetrics{}, apiPolicy(t, startTeamOnly), runlimit.Defaults{})
	rec, resp := postStart(t, h, `{"env":"stg","namespace":"other","workloads":["ok"]}`)
	if rec.Code != http.StatusForbidden || len(resp.Forbidden) != 1 || len(store.started) != 0 {
		t.Errorf("status=%d resp=%+v started=%v", rec.Code, resp, store.started)
	}
}

func TestStartHandler_ForbiddenDoesNotRevealExistence(t *testing.T) {
	t.Parallel()

	h := NewStartHandler(&startStore{}, &fakeMetrics{}, apiPolicy(t, startTeamOnly), runlimit.Defaults{})
	_, existing := postStart(t, h, `{"env":"stg","namespace":"other","workloads":["ok"]}`)
	_, missing := postStart(t, h, `{"env":"stg","namespace":"other","workloads":["nonexistent"]}`)
	if !slices.Equal(existing.Forbidden, []string{"ok"}) || !slices.Equal(missing.Forbidden, []string{"nonexistent"}) {
		t.Errorf("existing=%+v missing=%+v, want identical forbidden handling", existing, missing)
	}
}
