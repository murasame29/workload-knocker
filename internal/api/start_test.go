package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/murasame29/workload-knocker/internal/model"
)

// startStore starts only workloads named "ok".
type startStore struct{ fakeStore }

func (s *startStore) Clusters() []model.ClusterRef {
	return []model.ClusterRef{{Env: "stg", Name: "stg-cluster"}}
}

func (s *startStore) Start(_, _, name, _ string) bool { return name == "ok" }

func TestStartHandler_ObservesSuccessAndNotFound(t *testing.T) {
	t.Parallel()

	m := &fakeMetrics{}
	h := NewStartHandler(&startStore{}, m)
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
