package slackbot

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
)

type fakeGroups struct {
	mu      sync.Mutex
	members map[string][]string
	errs    map[string]error
	calls   map[string]int
}

func (f *fakeGroups) GetUserGroupMembersContext(_ context.Context, group string, _ ...slack.GetUserGroupMembersOption) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[group]++
	if err := f.errs[group]; err != nil {
		return nil, err
	}
	return f.members[group], nil
}

func policyHolder(t *testing.T, doc string) *authz.Holder {
	t.Helper()
	p, err := authz.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return authz.NewHolder(p)
}

const groupPolicy = `
version: 1
rules:
  - env: "*"
    namespaces: ["*"]
    subjects: {slack: {users: [UDIRECT11], groups: [SGOOD1111, SBAD22222]}}
`

func TestBot_Subject_ResolvesPolicyReferencedGroups(t *testing.T) {
	t.Parallel()

	api := &fakeGroups{
		members: map[string][]string{"SGOOD1111": {"UMEMBER11", "UOTHER111"}},
		errs:    map[string]error{"SBAD22222": errors.New("missing_scope")},
	}
	b := &Bot{authz: policyHolder(t, groupPolicy), groups: newGroupCache(api, time.Minute)}

	member := b.subject("UMEMBER11")
	if member.Platform != authz.PlatformSlack || member.User != "UMEMBER11" || !slices.Equal(member.Groups, []string{"SGOOD1111"}) {
		t.Errorf("member = %+v", member)
	}
	if got := b.subject("UNOBODY11"); len(got.Groups) != 0 {
		t.Errorf("non-member groups = %v", got.Groups)
	}
}

func TestBot_Subject_LookupFailureNeverFailsOpen(t *testing.T) {
	t.Parallel()

	api := &fakeGroups{errs: map[string]error{"SGOOD1111": errors.New("ratelimited"), "SBAD22222": errors.New("ratelimited")}}
	b := &Bot{authz: policyHolder(t, groupPolicy), groups: newGroupCache(api, time.Minute)}

	sub := b.subject("UDIRECT11")
	if len(sub.Groups) != 0 {
		t.Fatalf("groups = %v, want none after failed lookups", sub.Groups)
	}
	if !b.authz.Allowed(sub, "stg", "ns", "w", authz.ActionStart) {
		t.Error("direct user ID match must still work when group lookup fails")
	}
	viaGroupOnly := b.subject("UMEMBER11")
	if b.authz.Allowed(viaGroupOnly, "stg", "ns", "w", authz.ActionStart) {
		t.Error("a user allowed only through a group must be denied when lookup fails")
	}
}

func TestBot_Subject_SkipsLookupWithoutGroupRules(t *testing.T) {
	t.Parallel()

	api := &fakeGroups{}
	b := &Bot{
		authz:  policyHolder(t, "version: 1\nrules:\n  - {env: '*', namespaces: ['*'], subjects: {slack: {users: [UDIRECT11]}}}\n"),
		groups: newGroupCache(api, time.Minute),
	}
	b.subject("UDIRECT11")
	if len(api.calls) != 0 {
		t.Errorf("calls = %v, want no usergroups API call without group rules", api.calls)
	}
	if b2 := (&Bot{authz: authz.NewHolder(nil), groups: newGroupCache(api, time.Minute)}); len(b2.subject("U1").Groups) != 0 {
		t.Error("disabled authorization needs no groups")
	}
}

func TestGroupCache_TTLAndErrorsNotCached(t *testing.T) {
	t.Parallel()

	api := &fakeGroups{members: map[string][]string{"S1": {"UA"}}, errs: map[string]error{"S2": errors.New("boom")}}
	c := newGroupCache(api, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if ok, err := c.isMember(ctx, "S1", "UA"); err != nil || !ok {
			t.Fatalf("isMember = %v, %v", ok, err)
		}
	}
	if api.calls["S1"] != 1 {
		t.Errorf("calls within TTL = %d, want 1", api.calls["S1"])
	}
	now = now.Add(2 * time.Minute)
	if _, err := c.isMember(ctx, "S1", "UA"); err != nil {
		t.Fatal(err)
	}
	if api.calls["S1"] != 2 {
		t.Errorf("calls after TTL = %d, want 2", api.calls["S1"])
	}
	for range 2 {
		if _, err := c.isMember(ctx, "S2", "UA"); err == nil {
			t.Fatal("expected error")
		}
	}
	if api.calls["S2"] != 2 {
		t.Errorf("failed lookups must not be cached, calls = %d", api.calls["S2"])
	}
}

func TestRejectionAndApplyLines(t *testing.T) {
	t.Parallel()

	res := frontend.ApplyResult{
		Applied: []frontend.Applied{{Action: frontend.ActionStart, Name: "a", Found: true}},
		Rejected: []frontend.Rejection{
			{Action: frontend.ActionStop, Label: "b", Reason: frontend.ReasonChatStopDisabled},
			{Action: frontend.ActionStart, Label: "c", Reason: frontend.ReasonUnauthorized},
		},
	}
	got := strings.Join(applyLines(res), "\n")
	for _, want := range []string{"🟢 a → start", "⛔ b: stopping from chat is disabled", "nightly job", "⛔ c: you are not authorized to start this"} {
		if !strings.Contains(got, want) {
			t.Errorf("lines missing %q:\n%s", want, got)
		}
	}
}

func TestModalNote(t *testing.T) {
	t.Parallel()

	if got := modalNote(frontend.Listing{Entries: make([]frontend.Entry, 1)}, true); got != "" {
		t.Errorf("note = %q, want none", got)
	}
	if got := modalNote(frontend.Listing{}, true); !strings.Contains(got, "not authorized") {
		t.Errorf("empty listing note = %q", got)
	}
	if got := modalNote(frontend.Listing{Entries: make([]frontend.Entry, 1)}, false); !strings.Contains(got, "nightly job") {
		t.Errorf("scheduled-stop note = %q", got)
	}
}
