package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/frontend/notify"
	"github.com/murasame29/workload-knocker/internal/policy/runlimit"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

type notifyRecorder struct {
	enabled bool
	started []notify.Event
	stopped []notify.Event
}

func (r *notifyRecorder) Enabled(string) bool                                     { return r.enabled }
func (r *notifyRecorder) Started(_ context.Context, e notify.Event)               { r.started = append(r.started, e) }
func (r *notifyRecorder) Stopped(_ context.Context, e notify.Event)               { r.stopped = append(r.stopped, e) }
func (r *notifyRecorder) StopReminder(context.Context, model.Workload, time.Time) {}

func TestStartHandler_NotifiesStartedWorkloads(t *testing.T) {
	t.Parallel()

	n := &notifyRecorder{enabled: true}
	h := NewStartHandler(&auditStore{}, &fakeMetrics{}, allowAll{}, runlimit.Defaults{}, WithNotifier(n))
	postStart(t, h, `{"env":"stg","namespace":"ns","workloads":["ok","missing"],"user":"someone"}`)

	if len(n.started) != 1 {
		t.Fatalf("started events = %d, want 1", len(n.started))
	}
	e := n.started[0]
	if e.Kind != notify.KindStart || e.Actor != "api" || e.StartedBy != "someone" || e.Env != "stg" || e.ClusterName != "stg-cluster" || e.Namespace != "ns" {
		t.Errorf("event = %+v", e)
	}
	if len(e.Workloads) != 1 || e.Workloads[0].Name != "ok" {
		t.Errorf("workloads = %+v, want only the started one", e.Workloads)
	}
}

func TestStartHandler_NoNotifyWithoutChannelOrStart(t *testing.T) {
	t.Parallel()

	off := &notifyRecorder{}
	postStart(t, NewStartHandler(&auditStore{}, &fakeMetrics{}, allowAll{}, runlimit.Defaults{}, WithNotifier(off)), `{"env":"stg","namespace":"ns","workloads":["ok"]}`)
	on := &notifyRecorder{enabled: true}
	postStart(t, NewStartHandler(&auditStore{}, &fakeMetrics{}, allowAll{}, runlimit.Defaults{}, WithNotifier(on)), `{"env":"stg","namespace":"ns","workloads":["missing"]}`)
	if len(off.started) != 0 || len(on.started) != 0 {
		t.Errorf("off=%+v on=%+v, want no events", off.started, on.started)
	}
}

func TestStopAllHandler_NotifiesStoppedWorkloadsPerCluster(t *testing.T) {
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
	for _, tc := range []struct {
		header, actor string
	}{{"nightly-stop", "nightly-stop"}, {"", "api"}} {
		n := &notifyRecorder{enabled: true}
		res := model.StopAllResult{PerCluster: map[string]model.ClusterStopResult{"stg": {Stopped: 1}}}
		h := New(&auditStore{stopAllRes: res}, &fakeMetrics{}, apiPolicy(t, stopA), WithNotifier(n))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/stop-all", nil)
		if tc.header != "" {
			req.Header.Set(SourceHeader, tc.header)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)

		if len(n.stopped) != 1 {
			t.Fatalf("%q: stopped events = %d, want one per cluster", tc.header, len(n.stopped))
		}
		e := n.stopped[0]
		if e.Kind != notify.KindStopAll || e.Actor != tc.actor || e.Env != "stg" || len(e.Workloads) != 1 || e.Workloads[0].Name != "a" || e.Workloads[0].Namespace != "ns" {
			t.Errorf("%q: event = %+v (the denied workload b must not be listed)", tc.header, e)
		}
	}
}

func TestStopAllHandler_FailedClusterIsNotAnnounced(t *testing.T) {
	t.Parallel()

	n := &notifyRecorder{enabled: true}
	res := model.StopAllResult{PerCluster: map[string]model.ClusterStopResult{"stg": {Error: "boom"}}}
	New(&auditStore{stopAllRes: res}, &fakeMetrics{}, allowAll{}, WithNotifier(n)).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/stop-all", nil))
	if len(n.stopped) != 0 {
		t.Errorf("stopped = %+v, want none for a failed cluster", n.stopped)
	}
}
