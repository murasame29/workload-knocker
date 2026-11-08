package slackbot

import (
	"fmt"
	"slices"
	"strings"

	"github.com/murasame29/workload-knocker/internal/model"
)

// mergeableSuffixes are workload name suffixes merged into one checkbox.
//
// When one service is split into per-role Deployments (<service>-api / <service>-app / <service>-worker),
// users see it as a single service, so workloads sharing a base name are bundled and one check
// starts/stops all members. This also reduces option counts against the 10-item limit.
var mergeableSuffixes = []string{"api", "app", "worker"}

// workloadGroup is a set of workloads shown as one checkbox in the Slack Modal.
// Grouped=false means a single-member standalone workload.
type workloadGroup struct {
	// Key is the identifier for display and sorting: the shared base name when merged, else the workload name.
	Key string
	// Grouped reports whether multiple workloads were merged into one checkbox.
	Grouped bool
	// Members are the target workloads, sorted by name ascending.
	Members []*model.Workload
}

// groupWorkloads bundles workloads into Modal display units, sorted by Key ascending (a-z).
//
// Merging happens only when two or more workloads share a base name with a mergeable suffix;
// a lone one (e.g. only api) stays a standalone workload.
//
// The order is fixed not only for display but so the checkbox split boundary and the truncation point
// match between render and submit (same reason as sortedWorkloads).
func groupWorkloads(workloads []*model.Workload) []workloadGroup {
	byBase := make(map[string][]*model.Workload)
	var singles []*model.Workload
	for _, w := range workloads {
		base, ok := splitMergeableSuffix(w.Name)
		if !ok {
			singles = append(singles, w)
			continue
		}
		byBase[base] = append(byBase[base], w)
	}

	groups := make([]workloadGroup, 0, len(byBase)+len(singles))
	for base, members := range byBase {
		if len(members) < 2 {
			// Do not merge a single suffixed workload.
			singles = append(singles, members...)
			continue
		}
		groups = append(groups, workloadGroup{
			Key:     base,
			Grouped: true,
			Members: sortedWorkloads(members),
		})
	}
	for _, w := range singles {
		groups = append(groups, workloadGroup{Key: w.Name, Members: []*model.Workload{w}})
	}

	// Sort by Key ascending; when Keys collide (a workload named like the base exists),
	// stabilize by option value.
	slices.SortFunc(groups, func(a, b workloadGroup) int {
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.Value(), b.Value())
	})
	return groups
}

// renderedGroups limits the diff to the rendered count in private_metadata,
// so groups truncated by the display limit are not mistaken for "unchecked = stopped".
// A rendered of 0 (unset, from an old Modal) or >= the current count returns all groups.
func renderedGroups(groups []workloadGroup, rendered int) []workloadGroup {
	if rendered <= 0 || rendered >= len(groups) {
		return groups
	}
	return groups[:rendered]
}

// splitMergeableSuffix returns base when name has the form "<base>-<mergeable suffix>".
func splitMergeableSuffix(name string) (string, bool) {
	for _, sfx := range mergeableSuffixes {
		trailer := "-" + sfx
		if len(name) > len(trailer) && strings.HasSuffix(name, trailer) {
			return name[:len(name)-len(trailer)], true
		}
	}
	return "", false
}

// Value is the Slack option value: the name of the representative member (first by name).
// Packing all member names would exceed the 75-char option value limit, so submit
// treats the group as ON if any member name is selected.
func (g workloadGroup) Value() string { return g.Members[0].Name }

// Names returns the member workload names sorted by name.
func (g workloadGroup) Names() []string {
	out := make([]string, 0, len(g.Members))
	for _, w := range g.Members {
		out = append(out, w.Name)
	}
	return out
}

// runningCount returns the number of running (replicas > 0) members.
func (g workloadGroup) runningCount() int {
	n := 0
	for _, w := range g.Members {
		if w.Replicas > 0 {
			n++
		}
	}
	return n
}

// anyRunning reports whether at least one member is running; used for the initial checkbox selection.
// Unchecking on partial start (🟡) would make an unchanged Plan stop running members,
// so lean toward running and converge on starting the missing members.
func (g workloadGroup) anyRunning() bool { return g.runningCount() > 0 }

// suffixes returns the suffixes of the merged members sorted by name.
func (g workloadGroup) suffixes() []string {
	out := make([]string, 0, len(g.Members))
	for _, w := range g.Members {
		out = append(out, strings.TrimPrefix(w.Name, g.Key+"-"))
	}
	return out
}

// label returns the text shown on the checkbox.
//
//	standalone: same as workloadLabel ("🟢 name (N pod)" / "🔴 name")
//	merged:     "🟢 base (api, app, worker)"             ... all members running
//	            "🟡 base (api, app, worker) 2/3 running" ... partially running
//	            "🔴 base (api, app, worker)"             ... all members stopped
func (g workloadGroup) label() string {
	if !g.Grouped {
		return truncateOptionText(workloadLabel(g.Members[0]))
	}

	running := g.runningCount()
	icon := "🔴"
	switch {
	case running == len(g.Members):
		icon = "🟢"
	case running > 0:
		icon = "🟡"
	}

	text := fmt.Sprintf("%s %s (%s)", icon, g.Key, strings.Join(g.suffixes(), ", "))
	if running > 0 && running < len(g.Members) {
		text += fmt.Sprintf(" %d/%d running", running, len(g.Members))
	}
	return truncateOptionText(text)
}

// changeLabel returns the target notation for a preview line; names are the members changed now.
// For merged groups it lists only the changed suffixes (e.g. "checkout (app, worker)").
func (g workloadGroup) changeLabel(names []string) string {
	if len(names) == 0 {
		return g.Key
	}
	if !g.Grouped {
		return names[0]
	}
	sfx := make([]string, 0, len(names))
	for _, n := range names {
		sfx = append(sfx, strings.TrimPrefix(n, g.Key+"-"))
	}
	return fmt.Sprintf("%s (%s)", g.Key, strings.Join(sfx, ", "))
}

// truncateOptionText fits Slack option text within the character limit.
func truncateOptionText(s string) string {
	runes := []rune(s)
	if len(runes) <= slackOptionTextMaxLen {
		return s
	}
	return string(runes[:slackOptionTextMaxLen])
}
