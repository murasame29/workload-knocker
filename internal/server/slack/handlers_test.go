package slackbot

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/model"
)

// helperConfig returns the Cluster Registry config shared by handlers_test (validated).
func helperConfig(t *testing.T, cs ...config.Cluster) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Controller: config.Controller{Namespace: "workload-knocker"},
		Clusters:   cs,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return cfg
}

// TestRegistryLookup_UnknownCommandReturnsNotFound verifies an unregistered slash command
// cannot be resolved in the registry.
func TestRegistryLookup_UnknownCommandReturnsNotFound(t *testing.T) {
	t.Parallel()

	reg := helperConfig(t,
		config.Cluster{
			Env:       "stg",
			Name:      "stg-cluster",
			Provider:  config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
			Frontends: config.ClusterFrontends{Slack: &config.SlackFrontend{SlashCommand: "/workload-knocker-stg"}},
		},
	)

	if _, ok := reg.BySlashCommand("/unknown"); ok {
		t.Errorf("BySlashCommand(/unknown): got found, want not found")
	}
}

// TestRegistryLookup_KnownCommandResolvesCluster verifies a registered slash command returns the
// expected cluster, so the Env / Name that handleSlashCommand puts in the Modal
// private_metadata can be trusted.
func TestRegistryLookup_KnownCommandResolvesCluster(t *testing.T) {
	t.Parallel()

	want := config.Cluster{
		Env:       "stg",
		Name:      "example-staging",
		Provider:  config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
		Frontends: config.ClusterFrontends{Slack: &config.SlackFrontend{SlashCommand: "/workload-knocker-stg"}},
	}

	reg := helperConfig(t, want)

	got, ok := reg.BySlashCommand("/workload-knocker-stg")
	if !ok {
		t.Fatalf("BySlashCommand(/workload-knocker-stg): got not found, want found")
	}
	if got.Env != want.Env {
		t.Errorf("cluster.Env: got %q, want %q", got.Env, want.Env)
	}
	if got.Name != want.Name {
		t.Errorf("cluster.Name: got %q, want %q", got.Name, want.Name)
	}
}

// TestModalMetadata_RoundTrip verifies the cluster (= env) field survives JSON (de)serialization of
// modalMetadata stored in the Modal private_metadata.
func TestModalMetadata_RoundTrip(t *testing.T) {
	t.Parallel()

	original := modalMetadata{
		ChannelID: "C12345",
		UserID:    "U67890",
		Cluster:   "stg",
		Namespace: "inhouse-solutions",
	}

	bytes, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got modalMetadata
	if err := json.Unmarshal(bytes, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got != original {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, original)
	}
}

// TestModalMetadata_OmitsEmptyNamespace verifies omitempty keeps private_metadata small
// when namespace is unset.
func TestModalMetadata_OmitsEmptyNamespace(t *testing.T) {
	t.Parallel()

	meta := modalMetadata{
		ChannelID: "C12345",
		UserID:    "U67890",
		Cluster:   "stg",
	}

	bytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	if strings.Contains(string(bytes), "namespace") {
		t.Errorf("expected namespace to be omitted, got: %s", string(bytes))
	}
	if !strings.Contains(string(bytes), `"cluster":"stg"`) {
		t.Errorf("expected cluster to be present, got: %s", string(bytes))
	}
}

// TestPreviewMetadata_NameOnlyEncodingBuysHeadroom verifies that keeping only workload names in
// ToStart / ToStop fits a diff of more than 10 items in private_metadata (3000 chars).
// Full "cluster/namespace/name" tuples would exceed the limit, so this encoding is required.
func TestPreviewMetadata_NameOnlyEncodingBuysHeadroom(t *testing.T) {
	t.Parallel()

	const count = 50 // upper bound expected in real operation
	names := make([]string, 0, count)
	tuples := make([]string, 0, count)
	for i := range count {
		// workload name of a realistic length (40 chars)
		name := fmt.Sprintf("security-checksheet-search-app-%09d", i)
		names = append(names, name)
		tuples = append(tuples, "stg/inhouse-solutions/"+name)
	}

	base := previewMetadata{
		ChannelID: "C12345",
		UserID:    "U67890",
		Cluster:   "stg",
		Namespace: "inhouse-solutions",
	}

	nameOnly := base
	nameOnly.ToStart = names[:count/2]
	nameOnly.ToStop = names[count/2:]

	got, err := json.Marshal(nameOnly)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if len(got) > slackPrivateMetadataMaxLen {
		t.Errorf("name-only private_metadata = %d chars, want <= %d", len(got), slackPrivateMetadataMaxLen)
	}

	fullTuple := base
	fullTuple.ToStart = tuples[:count/2]
	fullTuple.ToStop = tuples[count/2:]

	legacy, err := json.Marshal(fullTuple)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if len(legacy) <= slackPrivateMetadataMaxLen {
		t.Errorf("full-tuple private_metadata = %d chars; expected it to exceed %d "+
			"(if this no longer holds, the name-only encoding rationale needs revisiting)",
			len(legacy), slackPrivateMetadataMaxLen)
	}
}

// TestWorkloadCheckboxBlocks_SplitsAtSlackLimit verifies blocks are split so the checkboxes
// 10-option limit is not exceeded and each workload appears exactly once.
func TestWorkloadCheckboxBlocks_SplitsAtSlackLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		count      int
		wantBlocks int
	}{
		{count: 0, wantBlocks: 0},
		{count: 1, wantBlocks: 1},
		{count: 10, wantBlocks: 1},
		{count: 11, wantBlocks: 2},
		{count: 20, wantBlocks: 2},
		{count: 25, wantBlocks: 3},
		{count: maxWorkloadOptions, wantBlocks: maxWorkloadCheckboxBlocks},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("count=%d", tc.count), func(t *testing.T) {
			t.Parallel()

			blocks := workloadCheckboxBlocks("ns", listingOf(makeWorkloads(tc.count)))
			if len(blocks) != tc.wantBlocks {
				t.Fatalf("block count = %d, want %d", len(blocks), tc.wantBlocks)
			}

			seenName := make(map[string]int)
			seenBlockID := make(map[string]bool)
			seenActionID := make(map[string]bool)

			for i, b := range blocks {
				input, ok := b.(*slack.InputBlock)
				if !ok {
					t.Fatalf("block %d: type = %T, want *slack.InputBlock", i, b)
				}
				if !input.Optional {
					t.Errorf("block %d: Optional = false, want true", i)
				}
				if seenBlockID[input.BlockID] {
					t.Errorf("block %d: duplicated block_id %q", i, input.BlockID)
				}
				seenBlockID[input.BlockID] = true
				if !strings.HasPrefix(input.BlockID, blockSelectWorkloads) {
					t.Errorf("block %d: block_id %q lacks prefix %q", i, input.BlockID, blockSelectWorkloads)
				}

				cb, ok := input.Element.(*slack.CheckboxGroupsBlockElement)
				if !ok {
					t.Fatalf("block %d: element type = %T, want *slack.CheckboxGroupsBlockElement", i, input.Element)
				}
				if seenActionID[cb.ActionID] {
					t.Errorf("block %d: duplicated action_id %q", i, cb.ActionID)
				}
				seenActionID[cb.ActionID] = true
				if !strings.HasPrefix(cb.ActionID, actionSelectWorkloads) {
					t.Errorf("block %d: action_id %q lacks prefix %q", i, cb.ActionID, actionSelectWorkloads)
				}
				if len(cb.Options) > checkboxMaxOptions {
					t.Errorf("block %d: %d options, want <= %d", i, len(cb.Options), checkboxMaxOptions)
				}
				for _, opt := range cb.Options {
					seenName[opt.Value]++
					if len(opt.Value) > 75 {
						t.Errorf("option value %q is %d chars, want <= 75", opt.Value, len(opt.Value))
					}
				}
			}

			if len(seenName) != tc.count {
				t.Errorf("distinct options = %d, want %d", len(seenName), tc.count)
			}
			for name, n := range seenName {
				if n != 1 {
					t.Errorf("option %q appeared %d times, want 1", name, n)
				}
			}
		})
	}
}

// TestWorkloadCheckboxBlocks_InitialOptionsReflectRunning verifies only running workloads go into
// initial_options; an empty slice is rejected by Slack, so it must be nil.
func TestWorkloadCheckboxBlocks_InitialOptionsReflectRunning(t *testing.T) {
	t.Parallel()

	workloads := makeWorkloads(15)
	// Make only even indexes running
	running := make(map[string]bool)
	for i, w := range workloads {
		if i%2 == 0 {
			w.Replicas = 1
			running[w.Name] = true
		}
	}

	gotInitial := make(map[string]bool)
	for _, b := range workloadCheckboxBlocks("ns", listingOf(workloads)) {
		cb := b.(*slack.InputBlock).Element.(*slack.CheckboxGroupsBlockElement)
		if cb.InitialOptions != nil && len(cb.InitialOptions) == 0 {
			t.Error("InitialOptions is an empty slice, want nil (Slack rejects [])")
		}
		for _, opt := range cb.InitialOptions {
			gotInitial[opt.Value] = true
		}
	}

	if len(gotInitial) != len(running) {
		t.Fatalf("initial options = %d, want %d", len(gotInitial), len(running))
	}
	for name := range running {
		if !gotInitial[name] {
			t.Errorf("running workload %q missing from initial_options", name)
		}
	}
}

// TestWorkloadCheckboxBlocks_FitsModalBlockLimit verifies rendering the max count fits
// the 100-block Modal limit.
func TestWorkloadCheckboxBlocks_FitsModalBlockLimit(t *testing.T) {
	t.Parallel()

	blocks := workloadCheckboxBlocks("ns", listingOf(makeWorkloads(maxWorkloadOptions)))
	total := len(blocks) + reservedModalBlocks // namespace select + warning section
	if total > slackModalMaxBlocks {
		t.Errorf("total blocks = %d, want <= %d", total, slackModalMaxBlocks)
	}
}

// TestSelectedWorkloadNames_CollectsAcrossBlocks verifies the selections of split checkbox blocks
// fold into one set with no unrelated blocks mixed in.
func TestSelectedWorkloadNames_CollectsAcrossBlocks(t *testing.T) {
	t.Parallel()

	values := map[string]map[string]slack.BlockAction{
		blockSelectWorkloads + "_0": {
			actionSelectWorkloads + "_0": {SelectedOptions: []slack.OptionBlockObject{
				{Value: "app-a"}, {Value: "app-b"},
			}},
		},
		blockSelectWorkloads + "_1": {
			actionSelectWorkloads + "_1": {SelectedOptions: []slack.OptionBlockObject{
				{Value: "app-c"},
			}},
		},
		// The old single block form is also collected by prefix match
		blockSelectWorkloads: {
			actionSelectWorkloads: {SelectedOptions: []slack.OptionBlockObject{
				{Value: "app-d"},
			}},
		},
		// The namespace select is excluded
		blockSelectNamespace: {
			actionSelectNamespace: {SelectedOption: slack.OptionBlockObject{Value: "ns"}},
		},
	}

	got := selectedWorkloadNames(values)
	want := []string{"app-a", "app-b", "app-c", "app-d"}

	if len(got) != len(want) {
		t.Fatalf("selected count = %d, want %d (%v)", len(got), len(want), got)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%q not selected", name)
		}
	}
	if got["ns"] {
		t.Error("namespace select value leaked into workload selection")
	}
}

// TestMrkdwnSections_RespectsTextLimit verifies sections are split within the 3000-char limit
// and all lines are kept.
func TestMrkdwnSections_RespectsTextLimit(t *testing.T) {
	t.Parallel()

	lines := make([]string, 0, maxWorkloadOptions)
	for i := range maxWorkloadOptions {
		lines = append(lines, fmt.Sprintf("🟢 security-checksheet-search-app-%09d: stopped → *start*", i))
	}

	blocks := mrkdwnSections("header", lines)
	if len(blocks) < 2 {
		t.Fatalf("block count = %d, want >= 2 (should have split)", len(blocks))
	}

	var joined string
	for i, b := range blocks {
		section, ok := b.(*slack.SectionBlock)
		if !ok {
			t.Fatalf("block %d: type = %T, want *slack.SectionBlock", i, b)
		}
		if len(section.Text.Text) > slackSectionTextMaxLen {
			t.Errorf("block %d: text is %d chars, want <= %d", i, len(section.Text.Text), slackSectionTextMaxLen)
		}
		joined += section.Text.Text
	}

	if !strings.Contains(joined, "header") {
		t.Error("header missing from rendered sections")
	}
	for _, line := range lines {
		if !strings.Contains(joined, line) {
			t.Errorf("line %q missing from rendered sections", line)
		}
	}
}

// listingOf groups workloads into a Slack-limited listing.
func listingOf(ws []*model.Workload) frontend.Listing {
	return frontend.Listing{Entries: frontend.GroupEntries(ws), Limits: listLimits}
}

// makeWorkloads creates n stopped workloads (names ascending).
func makeWorkloads(n int) []*model.Workload {
	out := make([]*model.Workload, 0, n)
	for i := range n {
		out = append(out, &model.Workload{
			Env:       "stg",
			Namespace: "ns",
			Name:      fmt.Sprintf("app-%03d", i),
			Kind:      "Deployment",
		})
	}
	return out
}

// TestModalTitle_FitsWithinSlackLimit verifies modalTitle stays within the Slack plain_text title limit
// (24 chars), preventing OpenView from failing with invalid_arguments.
func TestModalTitle_FitsWithinSlackLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cl   config.Cluster
	}{
		{
			name: "short env",
			cl:   config.Cluster{Env: "stg", Name: "example-staging"},
		},
		{
			name: "us env",
			cl:   config.Cluster{Env: "stg-us", Name: "example-staging"},
		},
		{
			name: "long env truncated",
			cl:   config.Cluster{Env: "extremely-long-env-key-that-overflows", Name: "n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			title := modalTitle(tc.cl)
			runeLen := len([]rune(title))
			if runeLen > slackModalTitleMaxLen {
				t.Errorf("modalTitle(%+v): %q has %d runes, want <= %d", tc.cl, title, runeLen, slackModalTitleMaxLen)
			}
		})
	}
}
