package frontend

import (
	"fmt"
	"slices"
	"strings"

	"github.com/murasame29/workload-knocker/internal/model"
)

// mergeableSuffixes are workload name suffixes merged into one entry.
// Per-role Deployments (<service>-api / -app / -worker) are one service to users.
var mergeableSuffixes = []string{"api", "app", "worker"}

// State is the aggregate running state of an entry.
type State int

const (
	StateStopped State = iota
	StatePartial
	StateRunning
)

// Entry is a set of workloads shown as one selectable item.
// Grouped=false means a single-member standalone workload.
type Entry struct {
	// Key is the shared base name when merged, else the workload name.
	Key     string
	Grouped bool
	// Members are sorted by name ascending.
	Members []*model.Workload
}

// GroupEntries bundles workloads into entries sorted by Key ascending (a-z).
// Workloads merge only when two or more share a base name with a mergeable suffix.
// The order is deterministic so page boundaries and truncation match between render and submit.
func GroupEntries(workloads []*model.Workload) []Entry {
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

	entries := make([]Entry, 0, len(byBase)+len(singles))
	for base, members := range byBase {
		if len(members) < 2 {
			singles = append(singles, members...)
			continue
		}
		entries = append(entries, Entry{Key: base, Grouped: true, Members: SortedWorkloads(members)})
	}
	for _, w := range singles {
		entries = append(entries, Entry{Key: w.Name, Members: []*model.Workload{w}})
	}

	// Break Key ties (a workload named like the base) by Value.
	slices.SortFunc(entries, func(a, b Entry) int {
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.Value(), b.Value())
	})
	return entries
}

// SortedWorkloads returns a copy of workloads sorted by name; the store returns map order.
func SortedWorkloads(ws []*model.Workload) []*model.Workload {
	out := slices.Clone(ws)
	slices.SortFunc(out, func(a, b *model.Workload) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// Rendered limits entries to the count a frontend rendered, so entries cut off by its
// display limit are not mistaken for "unchecked = stopped". 0 or >= len returns all.
func Rendered(entries []Entry, rendered int) []Entry {
	if rendered <= 0 || rendered >= len(entries) {
		return entries
	}
	return entries[:rendered]
}

func splitMergeableSuffix(name string) (string, bool) {
	for _, sfx := range mergeableSuffixes {
		trailer := "-" + sfx
		if len(name) > len(trailer) && strings.HasSuffix(name, trailer) {
			return name[:len(name)-len(trailer)], true
		}
	}
	return "", false
}

// Value is the name of the representative member (first by name).
// A selection counts as ON for the entry if any member name is selected.
func (e Entry) Value() string { return e.Members[0].Name }

// Names returns the member workload names sorted by name.
func (e Entry) Names() []string {
	out := make([]string, 0, len(e.Members))
	for _, w := range e.Members {
		out = append(out, w.Name)
	}
	return out
}

// RunningCount returns the number of running (replicas > 0) members.
func (e Entry) RunningCount() int {
	n := 0
	for _, w := range e.Members {
		if w.Replicas > 0 {
			n++
		}
	}
	return n
}

// State returns running when all members run, partial when some do, else stopped.
func (e Entry) State() State {
	running := e.RunningCount()
	switch {
	case running == len(e.Members):
		return StateRunning
	case running > 0:
		return StatePartial
	}
	return StateStopped
}

// AnyRunning reports whether at least one member runs. Frontends pre-select such entries so an
// unchanged plan does not stop partially running members.
func (e Entry) AnyRunning() bool { return e.RunningCount() > 0 }

// Suffixes returns the merged members' suffixes sorted by name.
func (e Entry) Suffixes() []string {
	out := make([]string, 0, len(e.Members))
	for _, w := range e.Members {
		out = append(out, strings.TrimPrefix(w.Name, e.Key+"-"))
	}
	return out
}

// Label returns the display text with a state icon:
//
//	standalone: "🟢 name (N pod)" / "🔴 name"
//	merged:     "🟢 base (api, app)", "🟡 base (api, app) 1/2 running", "🔴 base (api, app)"
func (e Entry) Label() string {
	if !e.Grouped {
		w := e.Members[0]
		if w.Replicas > 0 {
			return fmt.Sprintf("🟢 %s (%d pod)", w.Name, w.Replicas)
		}
		return fmt.Sprintf("🔴 %s", w.Name)
	}

	icon := "🔴"
	switch e.State() {
	case StateRunning:
		icon = "🟢"
	case StatePartial:
		icon = "🟡"
	}
	text := fmt.Sprintf("%s %s (%s)", icon, e.Key, strings.Join(e.Suffixes(), ", "))
	if e.State() == StatePartial {
		text += fmt.Sprintf(" %d/%d running", e.RunningCount(), len(e.Members))
	}
	return text
}

// ChangeLabel returns the target notation for a plan line; names are the members changed.
// Merged entries list only the changed suffixes (e.g. "checkout (app, worker)").
func (e Entry) ChangeLabel(names []string) string {
	if len(names) == 0 {
		return e.Key
	}
	if !e.Grouped {
		return names[0]
	}
	sfx := make([]string, 0, len(names))
	for _, n := range names {
		sfx = append(sfx, strings.TrimPrefix(n, e.Key+"-"))
	}
	return fmt.Sprintf("%s (%s)", e.Key, strings.Join(sfx, ", "))
}
