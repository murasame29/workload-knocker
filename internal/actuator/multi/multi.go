// Package multi combines several actuators for one cluster: each workload is driven by the
// actuator that discovered it, so one registry entry (and one chat command) can mix them.
package multi

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/murasame29/workload-knocker/internal/actuator"
)

// Member is one actuator of the combination, in priority order.
type Member struct {
	Name       string
	Discoverer actuator.Discoverer
	Actuator   actuator.Actuator
}

// Backend implements actuator.Discoverer and actuator.Actuator over its members.
type Backend struct {
	env     string
	members []Member

	mu sync.Mutex
	// owner maps namespace/name to the member index that discovered it; removed workloads stay
	// until Forget so the right member releases them.
	owner map[string]int
	// conflicts remembers the members already reported for a workload claimed twice.
	conflicts map[string]string
}

var (
	_ actuator.Discoverer = (*Backend)(nil)
	_ actuator.Actuator   = (*Backend)(nil)
)

// New combines members; earlier members win when several discover the same workload.
func New(env string, members ...Member) *Backend {
	return &Backend{env: env, members: members, owner: map[string]int{}, conflicts: map[string]string{}}
}

func key(w actuator.Workload) string { return w.Namespace + "/" + w.Name }

// Discover merges the workloads of every member. It fails when any member fails, so the store
// keeps its previous view instead of dropping that member's workloads.
func (b *Backend) Discover(ctx context.Context) ([]actuator.Workload, error) {
	type found struct {
		w   actuator.Workload
		idx int
	}
	var merged []found
	claimed := map[string]int{}
	for i, m := range b.members {
		items, err := m.Discoverer.Discover(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
		for _, w := range items {
			k := key(w)
			if first, ok := claimed[k]; ok {
				b.reportConflict(k, b.members[first].Name, m.Name)
				continue
			}
			claimed[k] = i
			merged = append(merged, found{w: w, idx: i})
		}
	}

	b.mu.Lock()
	var moved []found
	for _, f := range merged {
		k := key(f.w)
		if prev, ok := b.owner[k]; ok && prev != f.idx {
			moved = append(moved, found{w: f.w, idx: prev})
		}
		b.owner[k] = f.idx
	}
	for k := range b.conflicts {
		if _, ok := claimed[k]; !ok {
			delete(b.conflicts, k)
		}
	}
	b.mu.Unlock()

	// A workload that moved to another member is released by its previous one.
	for _, f := range moved {
		if err := b.members[f.idx].Actuator.Forget(ctx, f.w); err != nil {
			slog.Warn("forget on actuator change failed", slog.String("cluster.env", b.env),
				slog.String("namespace", f.w.Namespace), slog.String("workload", f.w.Name),
				slog.String("actuator", b.members[f.idx].Name), slog.String("error.message", err.Error()))
		}
	}

	out := make([]actuator.Workload, len(merged))
	for i, f := range merged {
		out[i] = f.w
	}
	return out, nil
}

func (b *Backend) reportConflict(k, kept, ignored string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conflicts[k] == ignored {
		return
	}
	b.conflicts[k] = ignored
	slog.Warn("workload claimed by several actuators; using the first", slog.String("cluster.env", b.env),
		slog.String("workload", k), slog.String("actuator", kept), slog.String("ignored_actuator", ignored))
}

func (b *Backend) member(w actuator.Workload) (Member, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i, ok := b.owner[key(w)]
	if !ok {
		return Member{}, false
	}
	return b.members[i], true
}

// Replicas asks the member that discovered w.
func (b *Backend) Replicas(ctx context.Context, w actuator.Workload) (int32, error) {
	m, ok := b.member(w)
	if !ok {
		return 0, actuator.ErrReplicasUnknown
	}
	return m.Discoverer.Replicas(ctx, w)
}

// Apply starts or stops w through the member that discovered it.
func (b *Backend) Apply(ctx context.Context, w actuator.Workload, running bool) error {
	m, ok := b.member(w)
	if !ok {
		return fmt.Errorf("workload %s was not discovered by any actuator", key(w))
	}
	return m.Actuator.Apply(ctx, w, running)
}

// Forget releases w through the member that discovered it and drops the ownership.
func (b *Backend) Forget(ctx context.Context, w actuator.Workload) error {
	m, ok := b.member(w)
	if !ok {
		return nil
	}
	err := m.Actuator.Forget(ctx, w)
	b.mu.Lock()
	delete(b.owner, key(w))
	b.mu.Unlock()
	return err
}
