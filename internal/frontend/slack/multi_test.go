package slackbot

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/policy/mode"
	"github.com/murasame29/workload-knocker/internal/workload/model"
	"github.com/murasame29/workload-knocker/internal/workload/rollout"
)

// multiStore serves workloads of several namespaces.
type multiStore struct {
	model.WorkloadStore
	byNS map[string][]*model.Workload
}

func (s *multiStore) Namespaces(string) []string {
	out := make([]string, 0, len(s.byNS))
	for ns := range s.byNS {
		out = append(out, ns)
	}
	return out
}
func (s *multiStore) WorkloadsByNamespace(_, ns string) []*model.Workload { return s.byNS[ns] }

func nsWL(ns string, replicas ...int) []*model.Workload {
	out := make([]*model.Workload, 0, len(replicas))
	for i, r := range replicas {
		out = append(out, &model.Workload{Env: "stg", Name: fmt.Sprintf("%s-w%02d", ns, i), Namespace: ns, Kind: "Deployment", Replicas: r})
	}
	return out
}

func multiBot(t *testing.T, byNS map[string][]*model.Workload, policy string) *Bot {
	t.Helper()
	var p *authz.Policy
	if policy != "" {
		var err error
		if p, err = authz.Parse([]byte(policy)); err != nil {
			t.Fatal(err)
		}
	}
	az := authz.NewHolder(p)
	svc := frontend.NewService(&multiStore{byNS: byNS}, metrics.New(), time.Second, frontend.WithAuthorizer(az), frontend.WithMode(mode.Full))
	return &Bot{svc: svc, authz: az}
}

var multiSub = authz.Subject{Platform: authz.PlatformSlack, User: "UTEST0001"}

// checked builds the view state of the multi-namespace modal: block -> checked option values.
func checked(sel map[string][]string) map[string]map[string]slack.BlockAction {
	out := map[string]map[string]slack.BlockAction{}
	for blockID, values := range sel {
		action := strings.Replace(blockID, blockNamespaceWorkloads, actionNamespaceWorkloads, 1)
		var opts []slack.OptionBlockObject
		for _, v := range values {
			opts = append(opts, slack.OptionBlockObject{Value: v})
		}
		out[blockID] = map[string]slack.BlockAction{action: {SelectedOptions: opts}}
	}
	return out
}

func TestMultiModal_OneGroupPerNamespace(t *testing.T) {
	t.Parallel()
	b := multiBot(t, map[string][]*model.Workload{"alpha": nsWL("alpha", 1, 0), "beta": nsWL("beta", make([]int, 12)...)}, "")
	modal, ok := b.multiModal(multiSub, statusCluster, []string{"alpha", "beta"}, "C1", "U1")
	if !ok {
		t.Fatal("modal does not fit")
	}
	var ids, labels []string
	for _, blk := range modal.Blocks.BlockSet {
		if in, ok := blk.(*slack.InputBlock); ok {
			ids = append(ids, in.BlockID)
			labels = append(labels, in.Label.Text)
		}
	}
	if want := []string{"block_ns_0_0", "block_ns_1_0", "block_ns_1_1", blockDuration}; !slices.Equal(ids, want) {
		t.Errorf("block IDs = %v, want %v", ids, want)
	}
	if want := []string{"alpha", "beta 1-10", "beta 11-12", "Run duration (hours)"}; !slices.Equal(labels, want) {
		t.Errorf("labels = %v, want %v", labels, want)
	}
	first := modal.Blocks.BlockSet[0].(*slack.InputBlock).Element.(*slack.CheckboxGroupsBlockElement)
	if len(first.InitialOptions) != 1 || first.InitialOptions[0].Value != "alpha-w00" {
		t.Errorf("initial = %+v, want the running entry", first.InitialOptions)
	}
	var meta modalMetadata
	if err := json.Unmarshal([]byte(modal.PrivateMetadata), &meta); err != nil || len(meta.Namespaces) != 2 || meta.Namespaces[1] != (nsMeta{Namespace: "beta", Rendered: 12}) {
		t.Errorf("metadata = %+v %v", meta, err)
	}
}

func TestMultiModal_FallsBackOverTheBlockLimit(t *testing.T) {
	t.Parallel()
	byNS := map[string][]*model.Workload{}
	var names []string
	for i := range 10 {
		ns := fmt.Sprintf("ns-%02d", i)
		byNS[ns] = nsWL(ns, make([]int, 91)...) // 10 blocks each: 100 in total
		names = append(names, ns)
	}
	if _, ok := multiBot(t, byNS, "").multiModal(multiSub, statusCluster, names, "C1", "U1"); ok {
		t.Error("modal over the block limit must fall back")
	}
	if _, ok := multiBot(t, map[string][]*model.Workload{}, "").multiModal(multiSub, statusCluster, []string{"empty"}, "C1", "U1"); ok {
		t.Error("no entries must fall back to the namespace select notice")
	}
}

func TestPlanNamespaces_PlansEachNamespaceAndRechecksPolicy(t *testing.T) {
	t.Parallel()
	const policy = `
version: 1
rules:
  - env: stg
    namespaces: ["alpha"]
    workloads: ["*"]
    actions: [start, stop]
    subjects: {slack: {users: ["UTEST0001"]}}
  - env: stg
    namespaces: ["beta"]
    workloads: ["beta-w00"]
    actions: [start]
    subjects: {slack: {users: ["UTEST0001"]}}
`
	b := multiBot(t, map[string][]*model.Workload{"alpha": nsWL("alpha", 1, 0), "beta": nsWL("beta", 0, 0)}, policy)
	meta := modalMetadata{Cluster: "stg", Namespaces: []nsMeta{{Namespace: "alpha", Rendered: 2}, {Namespace: "beta", Rendered: 2}}}
	// alpha: stop w00, start w01. beta: start w00 and a forged value for w01 (not rendered for U1).
	values := checked(map[string][]string{"block_ns_0_0": {"alpha-w01"}, "block_ns_1_0": {"beta-w00", "beta-w01"}})
	plans := b.planNamespaces(multiSub, statusCluster, meta, values, 0, "req-1")

	if len(plans) != 2 || !slices.Equal(plans[0].plan.ToStart, []string{"alpha-w01"}) || !slices.Equal(plans[0].plan.ToStop, []string{"alpha-w00"}) {
		t.Fatalf("alpha plan = %+v", plans[0].plan)
	}
	if !slices.Equal(plans[1].plan.ToStart, []string{"beta-w00"}) || len(plans[1].plan.ToStop) != 0 {
		t.Errorf("beta plan = %+v, want only the authorized start", plans[1].plan)
	}
	lines := multiPreviewLines(plans)
	for _, want := range []string{"*alpha*", "🟢 alpha-w01: stopped → *start*", "🔴 alpha-w00: running → *stop*", "*beta*", "🟢 beta-w00: stopped → *start*"} {
		if !slices.Contains(lines, want) {
			t.Errorf("preview lacks %q: %q", want, lines)
		}
	}
	if got := namespaceList(plans); got != "alpha, beta" {
		t.Errorf("namespaces = %q", got)
	}
}

func TestSelectedByNamespace_IgnoresOtherBlocks(t *testing.T) {
	t.Parallel()
	values := checked(map[string][]string{"block_ns_0_0": {"a"}, "block_ns_0_1": {"b"}, "block_ns_2_0": {"c"}})
	values[blockDuration] = map[string]slack.BlockAction{actionDuration: {Value: "2"}}
	values["block_workloads_0"] = map[string]slack.BlockAction{"select_workloads_0": {SelectedOptions: []slack.OptionBlockObject{{Value: "x"}}}}
	got := selectedByNamespace(values)
	if len(got) != 2 || !got[0]["a"] || !got[0]["b"] || !got[2]["c"] {
		t.Errorf("selected = %v", got)
	}
}

func TestPreviewMetadata_PartsAndLegacySingleNamespace(t *testing.T) {
	t.Parallel()
	legacy := previewMetadata{Namespace: "labs", ToStart: []string{"a"}, ToStop: []string{"b"}}
	if got := legacy.parts(); len(got) != 1 || got[0].Namespace != "labs" || !slices.Equal(got[0].ToStop, []string{"b"}) {
		t.Errorf("legacy parts = %+v", got)
	}
	multi := previewMetadata{Parts: []previewPart{{Namespace: "alpha", ToStart: []string{"x"}}, {Namespace: "beta", ToStop: []string{"y"}}}}
	raw, _ := json.Marshal(multi)
	var back previewMetadata
	if err := json.Unmarshal(raw, &back); err != nil || len(back.parts()) != 2 || back.parts()[1].Namespace != "beta" {
		t.Errorf("round trip = %+v %v", back, err)
	}
}

func TestResultLines_SplitsProgressPerNamespace(t *testing.T) {
	t.Parallel()
	applied := []appliedPart{
		{req: frontend.ApplyRequest{Namespace: "alpha"}, res: frontend.ApplyResult{Applied: []frontend.Applied{{Action: frontend.ActionStart, Name: "web", Found: true}}}},
		{req: frontend.ApplyRequest{Namespace: "beta"}, res: frontend.ApplyResult{Applied: []frontend.Applied{{Action: frontend.ActionStart, Name: "web", Found: true}}}},
	}
	snap := rollout.Snapshot{{Target: rollout.Target{Namespace: "alpha", Name: "web", Direction: rollout.Start}, State: rollout.StateStarted}}
	lines := resultLines(applied, snap, time.Minute)
	if len(lines) != 4 || lines[0] != "*alpha*" || !strings.Contains(lines[1], "started") || lines[2] != "*beta*" || lines[3] != "🟢 web → start" {
		t.Errorf("lines = %q", lines)
	}
	if got := appliedNamespaces(applied); got != "alpha, beta" {
		t.Errorf("namespaces = %q", got)
	}
}
