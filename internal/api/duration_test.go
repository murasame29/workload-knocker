package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

var apiNow = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

// durStore keeps StopAt like the real store.
type durStore struct {
	fakeStore
	mu sync.Mutex
	ws map[string]*model.Workload
}

func newDurStore(ws ...*model.Workload) *durStore {
	s := &durStore{ws: map[string]*model.Workload{}}
	for _, w := range ws {
		s.ws[w.Name] = w
	}
	return s
}

func (s *durStore) Clusters() []model.ClusterRef {
	return []model.ClusterRef{{Env: "stg", Name: "stg-cluster"}}
}

func (s *durStore) WorkloadsByNamespace(string, string) []*model.Workload {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Workload
	for _, w := range s.ws {
		c := *w
		out = append(out, &c)
	}
	return out
}

func (s *durStore) Start(_, _, name, _ string, stopAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.ws[name]
	if ok {
		w.Replicas, w.StopAt = 1, stopAt
	}
	return ok
}

func (s *durStore) SetStopAt(_, _, name string, stopAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.ws[name]
	if !ok || w.Replicas == 0 {
		return false
	}
	w.StopAt = stopAt
	return true
}

func wlDur(name string, replicas int, stopAt time.Time) *model.Workload {
	return &model.Workload{Name: name, Namespace: "ns", Kind: "Deployment", Replicas: replicas, StopAt: stopAt}
}

var testDefs = runlimit.Defaults{Default: 4 * time.Hour, Max: 8 * time.Hour}

func newStart(s *durStore) (*StartHandler, *recorderFake) {
	h := NewStartHandler(s, &fakeMetrics{}, allowAll{}, testDefs)
	h.now = func() time.Time { return apiNow }
	rec := &recorderFake{}
	h.audit = rec
	return h, rec
}

func TestStartHandler_DurationSetsStopAt(t *testing.T) {
	t.Parallel()

	s := newDurStore(wlDur("a", 0, time.Time{}), wlDur("b", 0, time.Time{}))
	h, rec := newStart(s)
	w, resp := postStart(t, h, `{"env":"stg","namespace":"ns","workloads":["a"],"duration":"90m"}`)
	if w.Code != http.StatusOK || !resp.StopAt["a"].Equal(apiNow.Add(90*time.Minute)) {
		t.Fatalf("status=%d resp=%+v", w.Code, resp)
	}
	if !s.ws["a"].StopAt.Equal(apiNow.Add(90 * time.Minute)) {
		t.Errorf("store stopAt = %v", s.ws["a"].StopAt)
	}
	if rec.recs[0].Duration != 90*time.Minute || !rec.recs[0].StopAt.Equal(apiNow.Add(90*time.Minute)) {
		t.Errorf("audit = %+v", rec.recs[0])
	}

	postStart(t, h, `{"env":"stg","namespace":"ns","workloads":["b"]}`)
	if !s.ws["b"].StopAt.Equal(apiNow.Add(4 * time.Hour)) {
		t.Errorf("an omitted duration must use the 4h default, got %v", s.ws["b"].StopAt)
	}
}

func TestStartHandler_InvalidDurationStartsNothing(t *testing.T) {
	t.Parallel()

	for _, d := range []string{"soon", "5m", "9h", "-1h", "3"} {
		s := newDurStore(wlDur("a", 0, time.Time{}))
		h, _ := newStart(s)
		w := rawStart(h, `{"env":"stg","namespace":"ns","workloads":["a"],"duration":"`+d+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("duration %q: status = %d, want 400", d, w.Code)
		}
		if s.ws["a"].Replicas != 0 {
			t.Errorf("duration %q: the workload was started", d)
		}
	}
}

func TestStartHandler_AnnotationMaxApplies(t *testing.T) {
	t.Parallel()

	a := wlDur("a", 0, time.Time{})
	a.MaxDuration = 2 * time.Hour
	s := newDurStore(a)
	h, _ := newStart(s)
	w := rawStart(h, `{"env":"stg","namespace":"ns","workloads":["a"],"duration":"3h"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "at most 2h") {
		t.Errorf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func postExtend(t *testing.T, h *ExtendHandler, body string) (*httptest.ResponseRecorder, extendResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/extend", strings.NewReader(body)))
	var resp extendResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func newExtend(s *durStore) (*ExtendHandler, *recorderFake) {
	h := NewExtendHandler(s, allowAll{}, testDefs)
	h.now = func() time.Time { return apiNow }
	rec := &recorderFake{}
	h.audit = rec
	return h, rec
}

func TestExtendHandler_MovesStopAt(t *testing.T) {
	t.Parallel()

	s := newDurStore(wlDur("a", 1, apiNow.Add(time.Hour)))
	h, rec := newExtend(s)
	w, resp := postExtend(t, h, `{"env":"stg","namespace":"ns","workloads":["a"],"duration":"1h"}`)
	if w.Code != http.StatusOK || !resp.Extended["a"].Equal(apiNow.Add(2*time.Hour)) {
		t.Fatalf("status=%d resp=%+v", w.Code, resp)
	}
	if !s.ws["a"].StopAt.Equal(apiNow.Add(2 * time.Hour)) {
		t.Errorf("store stopAt = %v", s.ws["a"].StopAt)
	}
	if r := rec.recs[0]; r.Event != audit.WorkloadExtend || r.Outcome != audit.Success || r.Duration != time.Hour || r.EndUser != "api" {
		t.Errorf("audit = %+v", r)
	}
}

func TestExtendHandler_Errors(t *testing.T) {
	t.Parallel()

	s := newDurStore(wlDur("near", 1, apiNow.Add(7*time.Hour+30*time.Minute)), wlDur("idle", 0, time.Time{}))
	h, _ := newExtend(s)
	cases := []struct {
		body string
		want int
	}{
		{`{"env":"stg","namespace":"ns","workloads":["near"],"duration":"1h"}`, http.StatusConflict},
		{`{"env":"stg","namespace":"ns","workloads":["idle"],"duration":"1h"}`, http.StatusConflict},
		{`{"env":"stg","namespace":"ns","workloads":["ghost"],"duration":"1h"}`, http.StatusNotFound},
		{`{"env":"nope","namespace":"ns","workloads":["near"],"duration":"1h"}`, http.StatusNotFound},
		{`{"env":"stg","namespace":"ns","workloads":["near"]}`, http.StatusBadRequest},
		{`{"env":"stg","namespace":"ns","workloads":["near"],"duration":"x"}`, http.StatusBadRequest},
		{`{"env":"stg","namespace":"ns","workloads":["near"],"duration":"1m"}`, http.StatusBadRequest},
		{`{"env":"stg","namespace":"ns","workloads":[],"duration":"1h"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		if w, _ := postExtend(t, h, c.body); w.Code != c.want {
			t.Errorf("%s: status = %d, want %d (%s)", c.body, w.Code, c.want, w.Body.String())
		}
	}
	if !s.ws["near"].StopAt.Equal(apiNow.Add(7*time.Hour + 30*time.Minute)) {
		t.Error("a rejected extension must not move the stop time")
	}
}

func TestExtendHandler_Forbidden(t *testing.T) {
	t.Parallel()

	s := newDurStore(wlDur("billing", 1, apiNow.Add(time.Hour)))
	h := NewExtendHandler(s, apiPolicy(t, startTeamOnly), testDefs)
	rec := &recorderFake{}
	h.audit = rec
	w, resp := postExtend(t, h, `{"env":"stg","namespace":"team-a","workloads":["billing"],"duration":"1h"}`)
	if w.Code != http.StatusForbidden || len(resp.Forbidden) != 1 || rec.recs[0].Outcome != audit.Denied {
		t.Errorf("status=%d resp=%+v audit=%+v", w.Code, resp, rec.recs)
	}
}

func TestExtendHandler_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	h, _ := newExtend(newDurStore())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/extend", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", rec.Code)
	}
}

func rawStart(h *StartHandler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/start", strings.NewReader(body)))
	return rec
}
