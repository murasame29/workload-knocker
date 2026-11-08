package slackbot

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/model"
)

// wl creates a test workload.
func wl(name string, replicas int) *model.Workload {
	return &model.Workload{Env: "stg", Name: name, Namespace: "labs", Kind: "Deployment", Replicas: replicas}
}

// TestGroupWorkloads_MergesSuffixVariants verifies workloads sharing a base name that differ only by suffix
// (api/app/worker) merge into one entry, ordered a-z.
func TestGroupWorkloads_MergesSuffixVariants(t *testing.T) {
	t.Parallel()

	// Pass in deliberately shuffled order so the test does not depend on store map iteration order.
	in := []*model.Workload{
		wl("checkout-worker", 0),
		wl("payments-api", 0), // a single suffixed workload: do not merge
		wl("menu-app", 0),
		wl("checkout-api", 0),
		wl("menu-api", 0),
		wl("bedrock-hello", 0), // no suffix
		wl("checkout-app", 0),
	}

	groups := groupWorkloads(in)

	wantKeys := []string{"bedrock-hello", "checkout", "menu", "payments-api"}
	if len(groups) != len(wantKeys) {
		t.Fatalf("groups: got %d, want %d (%v)", len(groups), len(wantKeys), groupKeys(groups))
	}
	for i, want := range wantKeys {
		if groups[i].Key != want {
			t.Errorf("groups[%d].Key: got %q, want %q (all=%v)", i, groups[i].Key, want, groupKeys(groups))
		}
	}

	byKey := make(map[string]workloadGroup, len(groups))
	for _, g := range groups {
		byKey[g.Key] = g
	}

	// checkout: the three api/app/worker members merge into one checkbox.
	mm := byKey["checkout"]
	if !mm.Grouped {
		t.Errorf("checkout: Grouped = false, want true")
	}
	if got := strings.Join(mm.suffixes(), ","); got != "api,app,worker" {
		t.Errorf("checkout: suffixes = %q, want %q", got, "api,app,worker")
	}
	// The option value is only the representative member (first by name) workload name.
	if got, want := mm.Value(), "checkout-api"; got != want {
		t.Errorf("checkout: Value() = %q, want %q", got, want)
	}

	// menu: api and app merge.
	if got := len(byKey["menu"].Members); got != 2 {
		t.Errorf("menu: members = %d, want 2", got)
	}

	// A lone suffixed workload stays a standalone entry named after the workload.
	sa := byKey["payments-api"]
	if sa.Grouped {
		t.Errorf("payments-api: Grouped = true, want false")
	}
	if got := sa.label(); got != "🔴 payments-api" {
		t.Errorf("payments-api: label() = %q", got)
	}

	// No suffix: standalone too.
	if byKey["bedrock-hello"].Grouped {
		t.Errorf("bedrock-hello: Grouped = true, want false")
	}
}

// TestGroupWorkloads_NamesCoverAllMembers verifies Names() of a merged entry lists every member,
// which is required so one check emits workload_knocker_activation for both app and api.
func TestGroupWorkloads_NamesCoverAllMembers(t *testing.T) {
	t.Parallel()

	groups := groupWorkloads([]*model.Workload{
		wl("checkout-app", 0),
		wl("checkout-api", 0),
		wl("checkout-worker", 0),
	})
	if len(groups) != 1 {
		t.Fatalf("groups: got %d, want 1", len(groups))
	}

	got := groups[0].Names()
	want := []string{"checkout-api", "checkout-app", "checkout-worker"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Names():\n got %v\nwant %v", got, want)
	}
}

// TestGroupWorkloads_IsDeterministic verifies the order is the same regardless of input order and the
// input slice is not mutated, so truncation matches between render and submit.
func TestGroupWorkloads_IsDeterministic(t *testing.T) {
	t.Parallel()

	a := []*model.Workload{wl("charlie-api", 0), wl("alpha", 0), wl("charlie-app", 0), wl("bravo", 0)}
	b := []*model.Workload{wl("bravo", 0), wl("charlie-app", 0), wl("alpha", 0), wl("charlie-api", 0)}

	if got, want := strings.Join(groupKeys(groupWorkloads(a)), "|"), "alpha|bravo|charlie"; got != want {
		t.Errorf("keys(a) = %q, want %q", got, want)
	}
	if got, want := strings.Join(groupKeys(groupWorkloads(b)), "|"), "alpha|bravo|charlie"; got != want {
		t.Errorf("keys(b) = %q, want %q", got, want)
	}
	if a[0].Name != "charlie-api" {
		t.Errorf("input was mutated: a[0] = %q, want %q", a[0].Name, "charlie-api")
	}
}

// TestRenderedGroups_ClampsToRenderedCount verifies entries truncated by the display limit are excluded from
// the diff and that an unset / oversized rendered does not panic.
// rendered counts post-merge entries, so an oversized value from an old Modal (raw workload count) must be safe.
func TestRenderedGroups_ClampsToRenderedCount(t *testing.T) {
	t.Parallel()

	groups := groupWorkloads([]*model.Workload{wl("a", 0), wl("b", 0), wl("c", 0)})

	cases := []struct {
		name     string
		rendered int
		want     int
	}{
		{name: "unset falls back to all", rendered: 0, want: 3},
		{name: "negative falls back to all", rendered: -1, want: 3},
		{name: "truncated", rendered: 2, want: 2},
		{name: "exact", rendered: 3, want: 3},
		{name: "stale larger count is clamped", rendered: 99, want: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := len(renderedGroups(groups, tc.rendered)); got != tc.want {
				t.Errorf("renderedGroups(_, %d) = %d groups, want %d", tc.rendered, got, tc.want)
			}
		})
	}
}

// TestWorkloadGroup_Label verifies the label of a merged entry reflects the running state.
func TestWorkloadGroup_Label(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []*model.Workload
		want string
	}{
		{
			name: "all running",
			in:   []*model.Workload{wl("menu-api", 1), wl("menu-app", 1)},
			want: "🟢 menu (api, app)",
		},
		{
			name: "partially running",
			in:   []*model.Workload{wl("menu-api", 1), wl("menu-app", 0)},
			want: "🟡 menu (api, app) 1/2 running",
		},
		{
			name: "all stopped",
			in:   []*model.Workload{wl("menu-api", 0), wl("menu-app", 0)},
			want: "🔴 menu (api, app)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			groups := groupWorkloads(tc.in)
			if len(groups) != 1 {
				t.Fatalf("groups: got %d, want 1", len(groups))
			}
			if got := groups[0].label(); got != tc.want {
				t.Errorf("label() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWorkloadGroup_Label_TruncatedToSlackLimit verifies a long base name stays within the Slack
// option text limit (75 chars).
func TestWorkloadGroup_Label_TruncatedToSlackLimit(t *testing.T) {
	t.Parallel()

	base := strings.Repeat("long-name-", 8) // 80 chars
	groups := groupWorkloads([]*model.Workload{wl(base+"-api", 1), wl(base+"-app", 0)})
	if len(groups) != 1 {
		t.Fatalf("groups: got %d, want 1", len(groups))
	}
	if runeLen := len([]rune(groups[0].label())); runeLen > slackOptionTextMaxLen {
		t.Errorf("label() has %d runes, want <= %d", runeLen, slackOptionTextMaxLen)
	}
}

// TestWorkloadGroup_ChangeLabel verifies the preview line lists only the changed suffixes.
func TestWorkloadGroup_ChangeLabel(t *testing.T) {
	t.Parallel()

	g := groupWorkloads([]*model.Workload{
		wl("checkout-api", 1), wl("checkout-app", 0), wl("checkout-worker", 0),
	})[0]

	if got, want := g.changeLabel([]string{"checkout-app", "checkout-worker"}), "checkout (app, worker)"; got != want {
		t.Errorf("changeLabel(partial) = %q, want %q", got, want)
	}

	single := groupWorkloads([]*model.Workload{wl("bedrock-hello", 0)})[0]
	if got, want := single.changeLabel([]string{"bedrock-hello"}), "bedrock-hello"; got != want {
		t.Errorf("changeLabel(single) = %q, want %q", got, want)
	}
}

// TestWorkloadGroup_AnyRunning verifies the checkbox is checked on partial start.
// Unchecking it would make an unchanged Plan stop the running members.
func TestWorkloadGroup_AnyRunning(t *testing.T) {
	t.Parallel()

	partial := groupWorkloads([]*model.Workload{wl("menu-api", 1), wl("menu-app", 0)})[0]
	if !partial.anyRunning() {
		t.Errorf("partial group: anyRunning() = false, want true")
	}

	stopped := groupWorkloads([]*model.Workload{wl("menu-api", 0), wl("menu-app", 0)})[0]
	if stopped.anyRunning() {
		t.Errorf("stopped group: anyRunning() = true, want false")
	}
}

// TestGroupWorkloads_ReducesCheckboxBlocks verifies merging reduces the number of splits
// against the 10-option limit.
func TestGroupWorkloads_ReducesCheckboxBlocks(t *testing.T) {
	t.Parallel()

	// 6 services x (api, app) = 12 workloads; merged = 6 entries = 1 block
	var in []*model.Workload
	for _, svc := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		in = append(in, wl(svc+"-api", 0), wl(svc+"-app", 0))
	}

	if got := len(workloadCheckboxBlocks("labs", groupWorkloads(in))); got != 1 {
		t.Errorf("merged: block count = %d, want 1", got)
	}
	// Without merging, 12 options would need 2 blocks (kept for contrast).
	if len(in) <= checkboxMaxOptions {
		t.Fatalf("test premise broken: %d workloads should exceed %d", len(in), checkboxMaxOptions)
	}
}

// TestNamespaceInputBlock_SortedAToZ verifies the namespace dropdown is sorted a-z
// and the selected namespace is the initial selection.
func TestNamespaceInputBlock_SortedAToZ(t *testing.T) {
	t.Parallel()

	in := []string{"labs", "inhouse-solutions", "gees", "acme"}
	block := namespaceInputBlock(in, "labs")

	sel, ok := block.Element.(*slack.SelectBlockElement)
	if !ok {
		t.Fatalf("block.Element: got %T, want *slack.SelectBlockElement", block.Element)
	}

	var got []string
	for _, opt := range sel.Options {
		got = append(got, opt.Value)
	}
	want := []string{"acme", "gees", "inhouse-solutions", "labs"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("namespace options:\n got %v\nwant %v", got, want)
	}

	if sel.InitialOption == nil || sel.InitialOption.Value != "labs" {
		t.Errorf("InitialOption: got %+v, want value=labs", sel.InitialOption)
	}

	// Do not mutate the caller slice (it may share the store cache).
	if in[0] != "labs" {
		t.Errorf("input slice was mutated: %v", in)
	}
}

func groupKeys(groups []workloadGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Key)
	}
	return out
}
