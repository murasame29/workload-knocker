package frontend

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
)

// readOnlyStore serves the status path; any cluster call or mutation fails the test.
type readOnlyStore struct {
	model.WorkloadStore
	t  *testing.T
	ns map[string][]*model.Workload
}

func (s *readOnlyStore) Namespaces(string) []string {
	var out []string
	for ns := range s.ns {
		out = append(out, ns)
	}
	return out
}

func (s *readOnlyStore) WorkloadsByNamespace(_, ns string) []*model.Workload { return s.ns[ns] }

func (s *readOnlyStore) Reconcile(context.Context, string) error {
	s.t.Error("the status path must not call Reconcile")
	return nil
}

func (s *readOnlyStore) ReconcileAll(context.Context) error {
	s.t.Error("the status path must not call ReconcileAll")
	return nil
}

func (s *readOnlyStore) Start(string, string, string, string, time.Time) bool {
	s.t.Error("the status path must not start")
	return false
}

func (s *readOnlyStore) Stop(string, string, string) bool {
	s.t.Error("the status path must not stop")
	return false
}

func running(ns, name string, stopAt time.Time) *model.Workload {
	return &model.Workload{
		Env: "stg", Namespace: ns, Name: name, Kind: "Deployment", Replicas: 1,
		StartedBy: "alice", StartedAt: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC), StopAt: stopAt,
	}
}

func stopped(ns, name string) *model.Workload {
	return &model.Workload{Env: "stg", Namespace: ns, Name: name, Kind: "Deployment"}
}

func statusService(t *testing.T, ws map[string][]*model.Workload, az authz.Authorizer, m mode.Mode) *Service {
	t.Helper()
	return NewService(&readOnlyStore{t: t, ns: ws}, metrics.New(), 0, WithAuthorizer(az), WithMode(m))
}

func entryNames(ns StatusNamespace) []string {
	var out []string
	for _, e := range ns.Entries {
		out = append(out, e.Entry.Key)
	}
	return out
}

func TestService_Status_GroupsByNamespaceAndFiltersByPolicy(t *testing.T) {
	t.Parallel()

	stopAt := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ws := map[string][]*model.Workload{
		"zeta":   {running("zeta", "web", stopAt), stopped("zeta", "idle")},
		"alpha":  {running("alpha", "shop-api", stopAt), stopped("alpha", "shop-app"), running("alpha", "secret", stopAt)},
		"hidden": {running("hidden", "x", stopAt)},
	}
	policy := authzFunc(func(_ authz.Subject, _, ns, name string, _ authz.Action) bool {
		return ns != "hidden" && name != "secret"
	})
	svc := statusService(t, ws, policy, mode.Full)

	st := svc.Status(alice, "stg")

	if len(st.Namespaces) != 2 || st.Namespaces[0].Name != "alpha" || st.Namespaces[1].Name != "zeta" {
		t.Fatalf("namespaces = %+v, want alpha then zeta (hidden is filtered)", st.Namespaces)
	}
	if got := entryNames(st.Namespaces[0]); !slices.Equal(got, []string{"shop"}) {
		t.Errorf("alpha entries = %v, want the shop group only (secret is filtered)", got)
	}
	if got := entryNames(st.Namespaces[1]); !slices.Equal(got, []string{"idle", "web"}) {
		t.Errorf("zeta entries = %v", got)
	}
	shop := st.Namespaces[0].Entries[0]
	if shop.Entry.State() != StatePartial || shop.StartedBy != "alice" || !shop.StopAt.Equal(stopAt) || shop.StartedAt.IsZero() {
		t.Errorf("shop = %+v, want partial, started by alice with a stop time", shop)
	}
	if idle := st.Namespaces[1].Entries[0]; idle.StartedBy != "" || !idle.StopAt.IsZero() || len(idle.StopNames) != 0 || len(idle.ExtendNames) != 0 {
		t.Errorf("a stopped entry carries no run data or buttons: %+v", idle)
	}
}

func TestService_Status_FullyHiddenIsEmpty(t *testing.T) {
	t.Parallel()

	deny := authzFunc(func(authz.Subject, string, string, string, authz.Action) bool { return false })
	svc := statusService(t, map[string][]*model.Workload{"a": {running("a", "web", time.Time{})}}, deny, mode.Full)
	if !svc.Status(alice, "stg").Empty() {
		t.Error("a subject without any permission must see an empty status")
	}
}

func TestService_Status_ButtonsFollowPolicyModeAndStopTime(t *testing.T) {
	t.Parallel()

	stopAt := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ws := func() map[string][]*model.Workload {
		return map[string][]*model.Workload{"labs": {running("labs", "timed", stopAt), running("labs", "forever", time.Time{})}}
	}
	startOnly := authzFunc(func(_ authz.Subject, _, _, _ string, a authz.Action) bool { return a == authz.ActionStart })

	cases := []struct {
		name                 string
		az                   authz.Authorizer
		m                    mode.Mode
		wantExtend, wantStop map[string][]string
	}{
		{"full", allowAll, mode.Full,
			map[string][]string{"timed": {"timed"}}, map[string][]string{"timed": {"timed"}, "forever": {"forever"}}},
		{"scheduled-stop hides Stop", allowAll, mode.ScheduledStop,
			map[string][]string{"timed": {"timed"}}, map[string][]string{}},
		{"start-only policy hides Stop", startOnly, mode.Full,
			map[string][]string{"timed": {"timed"}}, map[string][]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := statusService(t, ws(), tc.az, tc.m).Status(alice, "stg")
			gotExtend, gotStop := map[string][]string{}, map[string][]string{}
			for _, e := range st.Namespaces[0].Entries {
				if len(e.ExtendNames) > 0 {
					gotExtend[e.Entry.Key] = e.ExtendNames
				}
				if len(e.StopNames) > 0 {
					gotStop[e.Entry.Key] = e.StopNames
				}
			}
			if len(gotExtend) != len(tc.wantExtend) || !slices.Equal(gotExtend["timed"], tc.wantExtend["timed"]) {
				t.Errorf("extend names = %v, want %v (only entries with an auto-stop can be extended)", gotExtend, tc.wantExtend)
			}
			if len(gotStop) != len(tc.wantStop) || !slices.Equal(gotStop["timed"], tc.wantStop["timed"]) || !slices.Equal(gotStop["forever"], tc.wantStop["forever"]) {
				t.Errorf("stop names = %v, want %v", gotStop, tc.wantStop)
			}
		})
	}
}

func TestStatusRef_RoundTripAndInvalid(t *testing.T) {
	t.Parallel()

	want := StatusRef{Env: "stg", Namespace: "labs", Names: []string{"shop-api", "shop-app"}}
	got, err := DecodeStatusRef(EncodeStatusRef(want))
	if err != nil || got.Env != want.Env || got.Namespace != want.Namespace || !slices.Equal(got.Names, want.Names) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "{", `{"e":"stg","n":"labs"}`, `{"n":"labs","w":["a"]}`} {
		if _, err := DecodeStatusRef(bad); err == nil {
			t.Errorf("DecodeStatusRef(%q): want an error", bad)
		}
	}
}

func TestResultLines(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 8, 13, 0, 0, 0, time.UTC)
	ext := ExtendResult{
		Extended: []Extended{{Name: "a", StopAt: at}, {Name: "b", Err: errors.New("workload is not running")}},
		Rejected: []Rejection{{Action: ActionExtend, Label: "c", Reason: ReasonUnauthorized}},
	}
	got := ext.Lines(func(t time.Time) string { return t.Format("15:04") })
	want := []string{"⏰ a now stops at 13:00", "⚠ b: workload is not running", "⛔ c: you are not authorized to extend this"}
	if !slices.Equal(got, want) {
		t.Errorf("extend lines = %q, want %q", got, want)
	}

	stop := ApplyResult{
		Applied:  []Applied{{Action: ActionStop, Name: "a", Found: true}, {Action: ActionStop, Name: "b"}},
		Rejected: []Rejection{{Action: ActionStop, Label: "c", Reason: ReasonChatStopDisabled}},
	}
	lines := stop.StopLines()
	if len(lines) != 3 || lines[0] != "🔴 a → stop" || lines[1] != "⚠ b: not found" {
		t.Errorf("stop lines = %q", lines)
	}
}
