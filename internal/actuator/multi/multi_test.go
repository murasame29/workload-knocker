package multi

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/murasame29/workload-knocker/internal/actuator"
)

// fake is a Discoverer and Actuator that records its calls.
type fake struct {
	mu       sync.Mutex
	items    []actuator.Workload
	err      error
	replicas int32
	applied  []string
	forgot   []string
}

func (f *fake) Discover(context.Context) ([]actuator.Workload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.items), f.err
}

func (f *fake) Replicas(context.Context, actuator.Workload) (int32, error) { return f.replicas, nil }

func (f *fake) Apply(_ context.Context, w actuator.Workload, running bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := "stop"
	if running {
		state = "start"
	}
	f.applied = append(f.applied, state+":"+key(w))
	return nil
}

func (f *fake) Forget(_ context.Context, w actuator.Workload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, key(w))
	return nil
}

func wl(ns, name string) actuator.Workload { return actuator.Workload{Namespace: ns, Name: name} }

func newBackend(keda, direct *fake) *Backend {
	return New("flowers",
		Member{Name: "keda-external", Discoverer: keda, Actuator: keda},
		Member{Name: "direct", Discoverer: direct, Actuator: direct},
	)
}

func names(ws []actuator.Workload) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, key(w))
	}
	slices.Sort(out)
	return out
}

func TestDiscover_MergesMembersAndRoutesApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	keda := &fake{items: []actuator.Workload{wl("terraria", "terraria")}, replicas: 1}
	direct := &fake{items: []actuator.Workload{wl("terraria", "tommy-world")}, replicas: 0}
	b := newBackend(keda, direct)

	got, err := b.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{"terraria/terraria", "terraria/tommy-world"}; !slices.Equal(names(got), want) {
		t.Fatalf("Discover = %v, want %v", names(got), want)
	}

	if r, _ := b.Replicas(ctx, wl("terraria", "terraria")); r != 1 {
		t.Errorf("Replicas(terraria) = %d, want 1 from keda", r)
	}
	if err := b.Apply(ctx, wl("terraria", "terraria"), true); err != nil {
		t.Fatal(err)
	}
	if err := b.Apply(ctx, wl("terraria", "tommy-world"), true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keda.applied, []string{"start:terraria/terraria"}) || !slices.Equal(direct.applied, []string{"start:terraria/tommy-world"}) {
		t.Errorf("applied keda=%v direct=%v; each workload must go to its own actuator", keda.applied, direct.applied)
	}
}

func TestDiscover_FirstMemberWinsOnConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	keda := &fake{items: []actuator.Workload{wl("ns", "app")}}
	direct := &fake{items: []actuator.Workload{wl("ns", "app")}}
	b := newBackend(keda, direct)

	got, err := b.Discover(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("Discover = %v, %v; want the workload once", got, err)
	}
	if err := b.Apply(ctx, wl("ns", "app"), false); err != nil {
		t.Fatal(err)
	}
	if len(keda.applied) != 1 || len(direct.applied) != 0 {
		t.Errorf("applied keda=%v direct=%v; the first member must win", keda.applied, direct.applied)
	}
}

func TestDiscover_MemberErrorFailsWholeDiscovery(t *testing.T) {
	t.Parallel()
	keda := &fake{items: []actuator.Workload{wl("ns", "a")}}
	direct := &fake{err: errors.New("boom")}
	if _, err := newBackend(keda, direct).Discover(context.Background()); err == nil {
		t.Error("Discover: want an error when a member fails")
	}
}

func TestDiscover_MovedWorkloadIsForgottenByPreviousMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	keda := &fake{}
	direct := &fake{items: []actuator.Workload{wl("ns", "app")}}
	b := newBackend(keda, direct)
	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}

	direct.mu.Lock()
	direct.items = nil
	direct.mu.Unlock()
	keda.mu.Lock()
	keda.items = []actuator.Workload{wl("ns", "app")}
	keda.mu.Unlock()
	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(direct.forgot, []string{"ns/app"}) {
		t.Errorf("direct.forgot = %v; the previous member must release a moved workload", direct.forgot)
	}
	if err := b.Apply(ctx, wl("ns", "app"), true); err != nil {
		t.Fatal(err)
	}
	if len(keda.applied) != 1 || len(direct.applied) != 0 {
		t.Errorf("applied keda=%v direct=%v; a moved workload goes to its new member", keda.applied, direct.applied)
	}
}

func TestForget_UsesOwnerEvenAfterRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	keda := &fake{}
	direct := &fake{items: []actuator.Workload{wl("ns", "app")}}
	b := newBackend(keda, direct)
	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	direct.mu.Lock()
	direct.items = nil
	direct.mu.Unlock()
	if _, err := b.Discover(ctx); err != nil {
		t.Fatal(err)
	}

	if err := b.Forget(ctx, wl("ns", "app")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(direct.forgot, []string{"ns/app"}) {
		t.Errorf("direct.forgot = %v; a removed workload is released by the member that had it", direct.forgot)
	}
	if err := b.Apply(ctx, wl("ns", "app"), true); err == nil {
		t.Error("Apply after Forget: want an error for an unknown workload")
	}
	if err := b.Forget(ctx, wl("ns", "app")); err != nil {
		t.Errorf("second Forget: %v; want no error", err)
	}
}

func TestReplicas_UnknownWorkload(t *testing.T) {
	t.Parallel()
	if _, err := newBackend(&fake{}, &fake{}).Replicas(context.Background(), wl("ns", "x")); !errors.Is(err, actuator.ErrReplicasUnknown) {
		t.Errorf("Replicas = %v, want ErrReplicasUnknown", err)
	}
}
