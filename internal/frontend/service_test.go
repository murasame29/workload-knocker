package frontend

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/rollout"
)

// authzFunc adapts a function to authz.Authorizer.
type authzFunc func(s authz.Subject, env, ns, workload string, a authz.Action) bool

func (f authzFunc) Allowed(s authz.Subject, env, ns, workload string, a authz.Action) bool {
	return f(s, env, ns, workload, a)
}

var allowAll = authzFunc(func(authz.Subject, string, string, string, authz.Action) bool { return true })

var alice = authz.Subject{Platform: authz.PlatformSlack, User: "UALICE111"}

// fakeStore is an in-memory model.WorkloadStore recording Start / Stop calls.
type fakeStore struct {
	model.WorkloadStore
	workloads []*model.Workload
	calls     []string
}

func (f *fakeStore) Namespaces(string) []string { return []string{"zeta", "alpha"} }
func (f *fakeStore) WorkloadsByNamespace(string, string) []*model.Workload {
	return f.workloads
}
func (f *fakeStore) Start(env, ns, name, user string, _ time.Time) error {
	f.calls = append(f.calls, "start:"+name+":"+user)
	return f.result(name)
}

// result maps the test workload names to store outcomes.
func (f *fakeStore) result(name string) error {
	switch name {
	case "missing":
		return model.ErrWorkloadNotFound
	case "conflict":
		return &model.RefusedError{Reason: "replicas is managed by argocd-controller; remove replicas from the manifest"}
	}
	return nil
}
func (f *fakeStore) Stop(env, ns, name string) error {
	f.calls = append(f.calls, "stop:"+name)
	if name == "conflict" {
		return f.result(name)
	}
	return nil
}
func (f *fakeStore) Reconcile(context.Context, string) error { return nil }

func manyWorkloads(n int) []*model.Workload {
	out := make([]*model.Workload, 0, n)
	for i := range n {
		out = append(out, wl(fmt.Sprintf("app-%03d", i), 0))
	}
	return out
}

func TestService_Namespaces_SortedWithoutMutatingStore(t *testing.T) {
	t.Parallel()

	svc := NewService(&fakeStore{workloads: []*model.Workload{wl("a", 0)}}, metrics.New(), 0, WithAuthorizer(allowAll))
	got, total := svc.Namespaces(alice, "stg")
	if !slices.Equal(got, []string{"alpha", "zeta"}) || total != 2 {
		t.Errorf("Namespaces = %v (total %d)", got, total)
	}
}

func TestService_List_AppliesLimits(t *testing.T) {
	t.Parallel()

	svc := NewService(&fakeStore{workloads: manyWorkloads(25)}, metrics.New(), 0, WithAuthorizer(allowAll))

	l := svc.List(alice, "stg", "labs", Limits{PageSize: 10, MaxEntries: 22})
	if len(l.Entries) != 22 || l.Truncated != 3 || l.Total() != 25 {
		t.Fatalf("entries=%d truncated=%d total=%d", len(l.Entries), l.Truncated, l.Total())
	}
	pages := l.Pages()
	if len(pages) != 3 || len(pages[0]) != 10 || len(pages[2]) != 2 {
		t.Errorf("pages = %d (last %d), want 3 (last 2)", len(pages), len(pages[len(pages)-1]))
	}

	unlimited := svc.List(alice, "stg", "labs", Limits{})
	if unlimited.Truncated != 0 || len(unlimited.Pages()) != 1 {
		t.Errorf("unlimited: truncated=%d pages=%d", unlimited.Truncated, len(unlimited.Pages()))
	}
}

func TestEntry_State(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   []*model.Workload
		want State
	}{
		{[]*model.Workload{wl("m-api", 1), wl("m-app", 1)}, StateRunning},
		{[]*model.Workload{wl("m-api", 1), wl("m-app", 0)}, StatePartial},
		{[]*model.Workload{wl("m-api", 0), wl("m-app", 0)}, StateStopped},
	}
	for _, tc := range cases {
		if got := GroupEntries(tc.in)[0].State(); got != tc.want {
			t.Errorf("State() = %v, want %v", got, tc.want)
		}
	}
}

func TestBuildPlan_DiffsSelectionAgainstState(t *testing.T) {
	t.Parallel()

	entries := GroupEntries([]*model.Workload{
		wl("checkout-api", 1), wl("checkout-app", 0), wl("checkout-worker", 0), // partial, selected: start the rest
		wl("legacy", 1), // running, unselected: stop
		wl("steady", 1), // running, selected: no change
		wl("idle", 0),   // stopped, unselected: no change
	})
	selected := map[string]bool{"checkout-api": true, "steady": true}

	plan := BuildPlan(config.Cluster{Env: "stg"}, "labs", entries, selected)

	if want := []string{"checkout-app", "checkout-worker"}; !slices.Equal(plan.ToStart, want) {
		t.Errorf("ToStart = %v, want %v", plan.ToStart, want)
	}
	if want := []string{"legacy"}; !slices.Equal(plan.ToStop, want) {
		t.Errorf("ToStop = %v, want %v", plan.ToStop, want)
	}
	if len(plan.Changes) != 2 ||
		plan.Changes[0].Label != "checkout (app, worker)" || plan.Changes[0].Action != ActionStart ||
		plan.Changes[1].Label != "legacy" || plan.Changes[1].Action != ActionStop {
		t.Errorf("Changes = %+v", plan.Changes)
	}
	if plan.Empty() {
		t.Error("Empty() = true")
	}
}

func TestBuildPlan_NoChangeIsEmpty(t *testing.T) {
	t.Parallel()

	entries := GroupEntries([]*model.Workload{wl("a", 1), wl("b", 0)})
	if plan := BuildPlan(config.Cluster{}, "ns", entries, map[string]bool{"a": true}); !plan.Empty() {
		t.Errorf("plan = %+v, want empty", plan)
	}
}

func TestService_Apply_CallsStoreInOrder(t *testing.T) {
	t.Parallel()

	fs := &fakeStore{}
	svc := NewService(fs, metrics.New(), 0, WithAuthorizer(allowAll))

	res := svc.Apply(ApplyRequest{
		Cluster: config.Cluster{Env: "stg", Name: "c"}, Namespace: "labs", User: "u", Subject: alice,
		ToStart: []string{"a", "missing", ""}, ToStop: []string{"b"},
	})

	if want := []string{"start:a:u", "start:missing:u", "stop:b"}; !slices.Equal(fs.calls, want) {
		t.Errorf("calls = %v, want %v", fs.calls, want)
	}
	if len(res.Applied) != 3 || res.Applied[1].Found || !res.Applied[2].Found || res.Applied[2].Action != ActionStop {
		t.Errorf("Applied = %+v", res.Applied)
	}
}

func TestService_Apply_ShowsActuatorRefusals(t *testing.T) {
	t.Parallel()

	svc := NewService(&fakeStore{}, metrics.New(), 0, WithAuthorizer(allowAll))
	res := svc.Apply(ApplyRequest{
		Cluster: config.Cluster{Env: "stg", Name: "c"}, Namespace: "labs", User: "u", Subject: alice,
		ToStart: []string{"conflict", "a"}, ToStop: []string{"conflict"},
	})

	if len(res.Applied) != 1 || res.Applied[0].Name != "a" || !res.Applied[0].Found {
		t.Errorf("Applied = %+v, want only a", res.Applied)
	}
	want := "replicas is managed by argocd-controller; remove replicas from the manifest"
	if len(res.Rejected) != 2 {
		t.Fatalf("Rejected = %+v, want the refused start and stop", res.Rejected)
	}
	for i, a := range []Action{ActionStart, ActionStop} {
		r := res.Rejected[i]
		if r.Action != a || r.Label != "conflict" || r.Reason != ReasonRefused || r.Message() != want {
			t.Errorf("Rejected[%d] = %+v (message %q), want %s refusal %q", i, r, r.Message(), a, want)
		}
	}
	if lines := res.StopLines(); !slices.Contains(lines, "⛔ conflict: "+want) {
		t.Errorf("StopLines = %v, want the refusal line", lines)
	}
}

// onlyWorkloads allows alice exactly the listed actions on the listed workload names.
func onlyWorkloads(actions []authz.Action, names ...string) authz.Authorizer {
	return authzFunc(func(s authz.Subject, _, _, w string, a authz.Action) bool {
		return s.User == alice.User && slices.Contains(names, w) && slices.Contains(actions, a)
	})
}

func TestService_NilAuthorizerDeniesEverything(t *testing.T) {
	t.Parallel()

	fs := &fakeStore{workloads: []*model.Workload{wl("a", 1)}}
	svc := NewService(fs, metrics.New(), 0)

	if got, total := svc.Namespaces(alice, "stg"); len(got) != 0 || total != 2 {
		t.Errorf("Namespaces = %v (total %d), want none of 2", got, total)
	}
	res := svc.Apply(ApplyRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, ToStart: []string{"a"}, ToStop: []string{"a"}})
	if len(fs.calls) != 0 || len(res.Rejected) != 2 {
		t.Errorf("calls = %v rejected = %+v, want store untouched", fs.calls, res.Rejected)
	}
}

func TestService_ListAndNamespaces_HideWhatSubjectCannotOperate(t *testing.T) {
	t.Parallel()

	fs := &fakeStore{workloads: []*model.Workload{wl("mine", 0), wl("theirs", 1)}}
	svc := NewService(fs, metrics.New(), 0, WithAuthorizer(onlyWorkloads([]authz.Action{authz.ActionStart}, "mine")))

	l := svc.List(alice, "stg", "labs", Limits{})
	if len(l.Entries) != 1 || l.Entries[0].Key != "mine" {
		t.Fatalf("entries = %+v, want only mine", l.Entries)
	}
	if got, _ := svc.Namespaces(alice, "stg"); len(got) != 2 {
		t.Errorf("Namespaces = %v, want both (each holds mine)", got)
	}

	nothing := NewService(fs, metrics.New(), 0, WithAuthorizer(onlyWorkloads([]authz.Action{authz.ActionStart}, "other")))
	if got, total := nothing.Namespaces(alice, "stg"); len(got) != 0 || total != 2 {
		t.Errorf("Namespaces = %v (total %d), want none of 2", got, total)
	}
	stranger := authz.Subject{Platform: authz.PlatformSlack, User: "UOTHER999"}
	if l := svc.List(stranger, "stg", "labs", Limits{}); len(l.Entries) != 0 {
		t.Errorf("stranger sees %+v", l.Entries)
	}
}

func TestService_Plan_RejectsUnauthorizedChanges(t *testing.T) {
	t.Parallel()

	entries := GroupEntries([]*model.Workload{
		wl("checkout-api", 0), wl("checkout-app", 0), // start allowed only for api
		wl("legacy", 1), // stop not allowed
	})
	svc := NewService(&fakeStore{}, metrics.New(), 0,
		WithAuthorizer(onlyWorkloads([]authz.Action{authz.ActionStart}, "checkout-api")))

	plan := svc.Plan(alice, config.Cluster{Env: "stg"}, "labs", entries, map[string]bool{"checkout-api": true}, "")

	if !slices.Equal(plan.ToStart, []string{"checkout-api"}) || len(plan.ToStop) != 0 {
		t.Errorf("ToStart = %v ToStop = %v", plan.ToStart, plan.ToStop)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Label != "checkout (api)" {
		t.Errorf("Changes = %+v", plan.Changes)
	}
	if len(plan.Rejected) != 2 ||
		plan.Rejected[0].Action != ActionStart || !slices.Equal(plan.Rejected[0].Names, []string{"checkout-app"}) ||
		plan.Rejected[1].Action != ActionStop || !slices.Equal(plan.Rejected[1].Names, []string{"legacy"}) ||
		plan.Rejected[1].Reason != ReasonUnauthorized {
		t.Errorf("Rejected = %+v", plan.Rejected)
	}
}

func TestService_Apply_RechecksEverySubmittedEntry(t *testing.T) {
	t.Parallel()

	fs := &fakeStore{}
	svc := NewService(fs, metrics.New(), 0, WithAuthorizer(onlyWorkloads([]authz.Action{authz.ActionStart}, "ok")))

	res := svc.Apply(ApplyRequest{
		Cluster: config.Cluster{Env: "stg", Name: "c"}, Namespace: "labs", User: "alice", Subject: alice,
		ToStart: []string{"ok", "forged"}, ToStop: []string{"ok"},
	})

	if want := []string{"start:ok:alice"}; !slices.Equal(fs.calls, want) {
		t.Errorf("calls = %v, want %v", fs.calls, want)
	}
	if len(res.Rejected) != 2 || res.Rejected[0].Label != "forged" || res.Rejected[1].Action != ActionStop {
		t.Errorf("Rejected = %+v", res.Rejected)
	}
}

func TestService_ScheduledStopModeRejectsChatStops(t *testing.T) {
	t.Parallel()

	fs := &fakeStore{}
	svc := NewService(fs, metrics.New(), 0, WithAuthorizer(allowAll), WithMode(mode.ScheduledStop))
	if svc.ChatStopAllowed() {
		t.Fatal("ChatStopAllowed = true in scheduled-stop")
	}

	entries := GroupEntries([]*model.Workload{wl("up", 1), wl("down", 0)})
	plan := svc.Plan(alice, config.Cluster{Env: "stg"}, "labs", entries, map[string]bool{"down": true}, "")
	if !slices.Equal(plan.ToStart, []string{"down"}) || len(plan.ToStop) != 0 {
		t.Errorf("ToStart = %v ToStop = %v", plan.ToStart, plan.ToStop)
	}
	if len(plan.Rejected) != 1 || plan.Rejected[0].Reason != ReasonChatStopDisabled ||
		!strings.Contains(plan.Rejected[0].Message(), "nightly job") {
		t.Errorf("Rejected = %+v", plan.Rejected)
	}

	res := svc.Apply(ApplyRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, ToStop: []string{"up"}})
	if len(fs.calls) != 0 || len(res.Rejected) != 1 || res.Rejected[0].Reason != ReasonChatStopDisabled {
		t.Errorf("calls = %v rejected = %+v", fs.calls, res.Rejected)
	}
}

func TestService_OffAndFullModesAllowChatStops(t *testing.T) {
	t.Parallel()

	for _, m := range []mode.Mode{mode.Full, mode.Off} {
		fs := &fakeStore{}
		svc := NewService(fs, metrics.New(), 0, WithAuthorizer(allowAll), WithMode(m))
		svc.Apply(ApplyRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, ToStop: []string{"x"}})
		if !slices.Equal(fs.calls, []string{"stop:x"}) {
			t.Errorf("mode %s: calls = %v", m, fs.calls)
		}
	}
}

func TestService_ScheduledStopHidesStopOnlyWorkloads(t *testing.T) {
	t.Parallel()

	fs := &fakeStore{workloads: []*model.Workload{wl("stop-only", 1)}}
	svc := NewService(fs, metrics.New(), 0,
		WithAuthorizer(onlyWorkloads([]authz.Action{authz.ActionStop}, "stop-only")), WithMode(mode.ScheduledStop))
	if l := svc.List(alice, "stg", "labs", Limits{}); len(l.Entries) != 0 {
		t.Errorf("entries = %+v, want none: chat cannot stop in scheduled-stop", l.Entries)
	}
}

func TestService_RolloutTargets(t *testing.T) {
	t.Parallel()

	store := &fakeStore{workloads: []*model.Workload{
		{Name: "web", Namespace: "labs", Kind: "Deployment"},
		{Name: "db", Namespace: "labs", Kind: "StatefulSet", Replicas: 1},
	}}
	svc := NewService(store, metrics.New(), 0, WithAuthorizer(allowAll))
	res := ApplyResult{Applied: []Applied{
		{Action: ActionStart, Name: "web", Found: true},
		{Action: ActionStop, Name: "db", Found: true},
		{Action: ActionStart, Name: "missing", Found: false},
	}}

	got := svc.RolloutTargets(ApplyRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs"}, res)

	if len(got) != 2 || got[0].Kind != "Deployment" || got[0].Direction != rollout.Start || got[1].Kind != "StatefulSet" || got[1].Direction != rollout.Stop || got[1].Env != "stg" {
		t.Errorf("targets = %+v", got)
	}
}

func TestService_TrackIsNoopWithoutTracker(t *testing.T) {
	t.Parallel()

	svc := NewService(&fakeStore{}, metrics.New(), 0)
	called := false
	svc.Track(context.Background(), []rollout.Target{{Name: "a"}}, func(rollout.Snapshot) { called = true })

	if called || svc.RolloutEnabled() || svc.RolloutTimeout() != 0 {
		t.Errorf("called=%v enabled=%v timeout=%v, want a no-op", called, svc.RolloutEnabled(), svc.RolloutTimeout())
	}
}
