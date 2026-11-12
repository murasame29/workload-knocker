package slackbot

import (
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/workload/rollout"
)

func TestProgressLines(t *testing.T) {
	t.Parallel()

	res := frontend.ApplyResult{Applied: []frontend.Applied{
		{Action: frontend.ActionStart, Name: "a", Found: true},
		{Action: frontend.ActionStart, Name: "b", Found: true},
		{Action: frontend.ActionStop, Name: "c", Found: true},
		{Action: frontend.ActionStart, Name: "d", Found: false},
	}}
	snap := rollout.Snapshot{
		{Target: rollout.Target{Name: "a", Direction: rollout.Start}, State: rollout.StateStarted},
		{Target: rollout.Target{Name: "b", Direction: rollout.Start}, State: rollout.StateTimeout, Detail: "0/1 ready, ImagePullBackOff"},
		{Target: rollout.Target{Name: "c", Direction: rollout.Stop}, State: rollout.StateStopped},
	}

	got := strings.Join(progressLines(res, snap, 10*time.Minute), "\n")

	for _, want := range []string{"✅ a started", "⚠ b not ready after 10m (0/1 ready, ImagePullBackOff)", "⏹ c stopped", "🟢 d → start"} {
		if !strings.Contains(got, want) {
			t.Errorf("lines missing %q:\n%s", want, got)
		}
	}
	if plain := strings.Join(progressLines(res, nil, 0), "\n"); !strings.Contains(plain, "🟢 a → start") || !strings.Contains(plain, "🔴 c → stop") {
		t.Errorf("without a snapshot the plain lines remain:\n%s", plain)
	}
}
