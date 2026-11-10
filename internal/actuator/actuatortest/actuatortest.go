// Package actuatortest is a reusable contract suite for actuator.Discoverer and actuator.Actuator
// implementations. Each implementation runs it against fakes of its own backend.
package actuatortest

import (
	"context"
	"errors"
	"testing"

	"github.com/murasame29/workload-knocker/internal/actuator"
)

// Fixture drives the fake backend behind a Harness.
type Fixture interface {
	// Manage makes w a managed workload whose current replicas are replicas.
	Manage(w actuator.Workload, replicas int32)
	// Unmanage removes w from the managed set.
	Unmanage(w actuator.Workload)
	// BreakReplicas makes reading the replicas of w fail.
	BreakReplicas(w actuator.Workload)
	// Published reports the desired state last applied to w and whether any is published.
	Published(w actuator.Workload) (running, ok bool)
}

// Harness is one implementation under test and the fake backend it runs on.
type Harness struct {
	actuator.Discoverer
	actuator.Actuator
	Fixture
}

// Run runs the contract against a fresh Harness per subtest.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Helper()
	ctx := context.Background()
	wl := func(name string) actuator.Workload {
		return actuator.Workload{Namespace: "ns", Name: name, Kind: "Deployment"}
	}

	t.Run("Discover lists managed workloads and drops unmanaged ones", func(t *testing.T) {
		h := newHarness(t)
		h.Manage(wl("a"), 0)
		h.Manage(wl("b"), 0)
		if got := names(t, h); len(got) != 2 || !got["a"] || !got["b"] {
			t.Fatalf("Discover = %v, want a and b", got)
		}
		h.Unmanage(wl("b"))
		if got := names(t, h); len(got) != 1 || !got["a"] {
			t.Fatalf("Discover after removal = %v, want a only", got)
		}
	})

	t.Run("Discover reports namespace, name and kind", func(t *testing.T) {
		h := newHarness(t)
		h.Manage(wl("a"), 0)
		got, err := h.Discover(ctx)
		if err != nil || len(got) != 1 {
			t.Fatalf("Discover = %v, %v", got, err)
		}
		if got[0].Namespace != "ns" || got[0].Name != "a" || got[0].Kind != "Deployment" {
			t.Errorf("Discover[0] = %+v", got[0])
		}
	})

	t.Run("Replicas returns the current replicas", func(t *testing.T) {
		h := newHarness(t)
		h.Manage(wl("on"), 3)
		h.Manage(wl("off"), 0)
		for name, want := range map[string]int32{"on": 3, "off": 0} {
			got, err := h.Replicas(ctx, wl(name))
			if errors.Is(err, actuator.ErrReplicasUnknown) {
				t.Skip("implementation does not report replicas")
			}
			if err != nil || got != want {
				t.Errorf("Replicas(%s) = %d, %v, want %d", name, got, err, want)
			}
		}
	})

	t.Run("Replicas fails for an unreadable target", func(t *testing.T) {
		h := newHarness(t)
		h.Manage(wl("a"), 1)
		h.BreakReplicas(wl("a"))
		_, err := h.Replicas(ctx, wl("a"))
		if err == nil {
			t.Fatal("Replicas succeeded, want an error")
		}
		if errors.Is(err, actuator.ErrReplicasUnknown) {
			t.Skip("implementation does not report replicas")
		}
	})

	t.Run("Apply starts and stops idempotently", func(t *testing.T) {
		h := newHarness(t)
		h.Manage(wl("a"), 0)
		for _, running := range []bool{true, true, false, false, true} {
			if err := h.Apply(ctx, wl("a"), running); err != nil {
				t.Fatalf("Apply(%v): %v", running, err)
			}
			if got, ok := h.Published(wl("a")); !ok || got != running {
				t.Fatalf("after Apply(%v): published = %v, %v", running, got, ok)
			}
		}
	})

	t.Run("Apply keeps workloads independent", func(t *testing.T) {
		h := newHarness(t)
		h.Manage(wl("a"), 0)
		h.Manage(wl("b"), 0)
		if err := h.Apply(ctx, wl("a"), true); err != nil {
			t.Fatal(err)
		}
		if err := h.Apply(ctx, wl("b"), false); err != nil {
			t.Fatal(err)
		}
		a, _ := h.Published(wl("a"))
		b, _ := h.Published(wl("b"))
		if !a || b {
			t.Errorf("published a=%v b=%v, want a=true b=false", a, b)
		}
	})

	t.Run("Forget releases the published state", func(t *testing.T) {
		h := newHarness(t)
		h.Manage(wl("a"), 0)
		if err := h.Apply(ctx, wl("a"), true); err != nil {
			t.Fatal(err)
		}
		if err := h.Forget(ctx, wl("a")); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if _, ok := h.Published(wl("a")); ok {
			t.Error("state still published after Forget")
		}
		if err := h.Forget(ctx, wl("a")); err != nil {
			t.Errorf("second Forget: %v", err)
		}
	})
}

func names(t *testing.T, h Harness) map[string]bool {
	t.Helper()
	items, err := h.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out := map[string]bool{}
	for _, it := range items {
		out[it.Name] = true
	}
	return out
}
