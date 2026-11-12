package slackbot

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

// wl creates a test workload.
func wl(name string, replicas int) *model.Workload {
	return &model.Workload{Env: "stg", Name: name, Namespace: "labs", Kind: "Deployment", Replicas: replicas}
}

// TestTruncateOptionText_FitsSlackLimit verifies a long base name stays within the Slack
// option text limit (75 chars).
func TestTruncateOptionText_FitsSlackLimit(t *testing.T) {
	t.Parallel()

	base := strings.Repeat("long-name-", 8) // 80 chars
	groups := frontend.GroupEntries([]*model.Workload{wl(base+"-api", 1), wl(base+"-app", 0)})
	if len(groups) != 1 {
		t.Fatalf("groups: got %d, want 1", len(groups))
	}
	if runeLen := len([]rune(truncateOptionText(groups[0].Label()))); runeLen > slackOptionTextMaxLen {
		t.Errorf("label() has %d runes, want <= %d", runeLen, slackOptionTextMaxLen)
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

	if got := len(workloadCheckboxBlocks("labs", listingOf(in))); got != 1 {
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
