package frontend

import (
	"context"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend/notify"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
	"github.com/murasame29/workload-knocker/internal/workload/model"
	"github.com/murasame29/workload-knocker/internal/workload/rollout"
)

// recordingNotifier records the events the service reports.
type recordingNotifier struct {
	enabled bool
	started []notify.Event
	stopped []notify.Event
}

func (r *recordingNotifier) Enabled(string) bool { return r.enabled }
func (r *recordingNotifier) Started(_ context.Context, e notify.Event) {
	r.started = append(r.started, e)
}
func (r *recordingNotifier) Stopped(_ context.Context, e notify.Event) {
	r.stopped = append(r.stopped, e)
}
func (r *recordingNotifier) StopReminder(context.Context, model.Workload, time.Time) {
}

func notifyService(n notify.Notifier, ws ...*model.Workload) *Service {
	return NewService(&fakeStore{workloads: ws}, metrics.New(), 0, WithAuthorizer(allowAll), WithNotifier(n))
}

func TestService_Apply_NotifiesStartAndStopOncePerAction(t *testing.T) {
	t.Parallel()

	n := &recordingNotifier{enabled: true}
	svc := notifyService(n, &model.Workload{Name: "a", Namespace: "labs", ReadyMessage: "go {{.Name}}"}, &model.Workload{Name: "b", Namespace: "labs", Replicas: 1})

	svc.Apply(ApplyRequest{
		Cluster: config.Cluster{Env: "stg", Name: "c"}, Namespace: "labs", User: "alice", Subject: alice,
		ToStart: []string{"a", "missing"}, ToStop: []string{"b"},
	})

	if len(n.started) != 1 || len(n.stopped) != 1 {
		t.Fatalf("started=%d stopped=%d, want one each", len(n.started), len(n.stopped))
	}
	st := n.started[0]
	if st.Kind != notify.KindStart || st.Actor != "<@UALICE111>" || st.StartedBy != "alice" || st.Env != "stg" || st.ClusterName != "c" || st.Namespace != "labs" {
		t.Errorf("start event = %+v", st)
	}
	if len(st.Workloads) != 1 || st.Workloads[0].Name != "a" || st.Workloads[0].ReadyMessage != "go {{.Name}}" {
		t.Errorf("start workloads (not-found ones are left out) = %+v", st.Workloads)
	}
	if sp := n.stopped[0]; sp.Kind != notify.KindStop || len(sp.Workloads) != 1 || sp.Workloads[0].Name != "b" {
		t.Errorf("stop event = %+v", sp)
	}
}

func TestService_Apply_SkipsNotifyWithoutChannelOrAuthorization(t *testing.T) {
	t.Parallel()

	off := &recordingNotifier{}
	notifyService(off, &model.Workload{Name: "a", Namespace: "labs"}).Apply(ApplyRequest{
		Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, ToStart: []string{"a"},
	})
	if len(off.started) != 0 {
		t.Errorf("a cluster without a channel must not be notified: %+v", off.started)
	}

	on := &recordingNotifier{enabled: true}
	svc := NewService(&fakeStore{workloads: []*model.Workload{{Name: "a", Namespace: "labs"}}}, metrics.New(), 0,
		WithAuthorizer(onlyWorkloads(nil)), WithNotifier(on))
	svc.Apply(ApplyRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, ToStart: []string{"a"}})
	if len(on.started) != 0 {
		t.Errorf("a rejected start must not be announced: %+v", on.started)
	}
}

func TestService_DefaultExtenderIsUnavailable(t *testing.T) {
	t.Parallel()

	svc := NewService(&fakeStore{}, metrics.New(), 0)
	if _, err := svc.Extender().Extend(context.Background(), notify.ExtendRequest{}); err == nil {
		t.Error("the default extender must refuse until the run-duration feature is wired in")
	}
}

func TestReadyRenderer_DecoratesStartedItemsOnce(t *testing.T) {
	t.Parallel()

	svc := notifyService(nil, &model.Workload{Name: "web", Namespace: "labs", ReadyMessage: "Connect to {{.Name}} ({{.StartedBy}})"},
		&model.Workload{Name: "bad", Namespace: "labs", ReadyMessage: "{{.Nope}}"}, &model.Workload{Name: "plain", Namespace: "labs"})
	r := svc.NewReadyRenderer(ApplyRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", User: "alice"})

	target := func(name string) rollout.Target {
		return rollout.Target{Env: "stg", Namespace: "labs", Name: name, Direction: rollout.Start}
	}
	snap := r.Decorate(rollout.Snapshot{
		{Target: target("web"), State: rollout.StateStarted},
		{Target: target("bad"), State: rollout.StateStarted},
		{Target: target("plain"), State: rollout.StateStarted},
		{Target: target("web"), State: rollout.StateStarting},
		{Target: rollout.Target{Name: "web", Direction: rollout.Stop}, State: rollout.StateStopped},
	})

	want := []string{"Connect to web (alice)", "", "", "", ""}
	for i, it := range snap {
		if it.Detail != want[i] {
			t.Errorf("item %d detail = %q, want %q", i, it.Detail, want[i])
		}
	}
	if got := rollout.Line(snap[0], time.Minute); got != "✅ web started — Connect to web (alice)" {
		t.Errorf("line = %q", got)
	}
	if got := rollout.Line(snap[1], time.Minute); got != "✅ bad started" {
		t.Errorf("a broken template falls back to the plain line, got %q", got)
	}
}
