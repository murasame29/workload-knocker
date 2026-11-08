package frontend

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

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
func (f *fakeStore) Start(env, ns, name, user string) bool {
	f.calls = append(f.calls, "start:"+name+":"+user)
	return name != "missing"
}
func (f *fakeStore) Stop(env, ns, name string) bool {
	f.calls = append(f.calls, "stop:"+name)
	return true
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

	svc := NewService(&fakeStore{}, metrics.New(), 0)
	if got := svc.Namespaces("stg"); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Errorf("Namespaces = %v", got)
	}
}

func TestService_List_AppliesLimits(t *testing.T) {
	t.Parallel()

	svc := NewService(&fakeStore{workloads: manyWorkloads(25)}, metrics.New(), 0)

	l := svc.List("stg", "labs", Limits{PageSize: 10, MaxEntries: 22})
	if len(l.Entries) != 22 || l.Truncated != 3 || l.Total() != 25 {
		t.Fatalf("entries=%d truncated=%d total=%d", len(l.Entries), l.Truncated, l.Total())
	}
	pages := l.Pages()
	if len(pages) != 3 || len(pages[0]) != 10 || len(pages[2]) != 2 {
		t.Errorf("pages = %d (last %d), want 3 (last 2)", len(pages), len(pages[len(pages)-1]))
	}

	unlimited := svc.List("stg", "labs", Limits{})
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
	svc := NewService(fs, metrics.New(), 0)

	res := svc.Apply(ApplyRequest{
		Cluster: config.Cluster{Env: "stg", Name: "c"}, Namespace: "labs", User: "u",
		ToStart: []string{"a", "missing", ""}, ToStop: []string{"b"},
	})

	if want := []string{"start:a:u", "start:missing:u", "stop:b"}; !slices.Equal(fs.calls, want) {
		t.Errorf("calls = %v, want %v", fs.calls, want)
	}
	if len(res.Applied) != 3 || res.Applied[1].Found || !res.Applied[2].Found || res.Applied[2].Action != ActionStop {
		t.Errorf("Applied = %+v", res.Applied)
	}
	if !res.AutoStop {
		t.Error("AutoStop = false with starts")
	}
}
