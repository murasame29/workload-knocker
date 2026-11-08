package discordbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/rollout"
)

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }
func (c *stepClock) Sleep(_ context.Context, d time.Duration) bool {
	c.now = c.now.Add(d)
	return true
}

// seqReader returns the given replica states one per poll, repeating the last.
type seqReader struct {
	states []k8s.ReplicaStatus
	issue  string
	i      int
}

func (r *seqReader) Replicas(context.Context, string, string, string) (k8s.ReplicaStatus, error) {
	s := r.states[min(r.i, len(r.states)-1)]
	r.i++
	return s, nil
}
func (r *seqReader) PodIssue(context.Context, string, string) (string, error) { return r.issue, nil }

type oneCluster struct{ r k8s.RolloutReader }

func (o oneCluster) Reader(context.Context, string) (k8s.RolloutReader, error) { return o.r, nil }

func confirmWithTracker(t *testing.T, ws []*model.Workload, toStart, toStop []string, reader k8s.RolloutReader) (*fakeSession, *Bot) {
	t.Helper()
	b, sess, _ := newTestBot(t, ws)
	tracker := rollout.NewTracker(oneCluster{reader}, 5*time.Second, 10*time.Minute, rollout.WithClock(&stepClock{now: time.Unix(0, 0)}))
	b.svc = frontend.NewService(&fakeStore{workloads: ws}, metrics.New(), time.Second,
		frontend.WithAuthorizer(authz.NewHolder(nil)), frontend.WithMode(mode.Full), frontend.WithRollout(tracker))
	token, err := b.flows.create(&flow{userID: "u1", env: testCluster().Env, namespace: "labs", toStart: toStart, toStop: toStop})
	if err != nil {
		t.Fatal(err)
	}
	b.dispatch(component("u1", customID{kindConfirm, token, 0}.String()))
	b.watchers.Wait()
	return sess, b
}

func editContents(s *fakeSession) []string {
	out := make([]string, len(s.edits))
	for i, e := range s.edits {
		out[i] = *e.Content
	}
	return out
}

func TestConfirm_ShowsStartingThenStarted(t *testing.T) {
	t.Parallel()

	reader := &seqReader{states: []k8s.ReplicaStatus{{Desired: 0}, {Desired: 1}, {Desired: 1, Ready: 1}}}
	sess, _ := confirmWithTracker(t, []*model.Workload{wl("terraria", 0)}, []string{"terraria"}, nil, reader)

	got := editContents(sess)
	if len(got) != 2 || !strings.Contains(got[0], "⏳ terraria starting...") || !strings.Contains(got[1], "✅ terraria started") {
		t.Fatalf("edits = %q, want starting then started", got)
	}
	if !strings.Contains(got[1], "<@u1> changed workloads") || strings.Contains(got[1], "22:00 JST") {
		t.Errorf("final edit keeps the header and has no fixed stop time: %q", got[1])
	}
}

func TestConfirm_ShowsStopped(t *testing.T) {
	t.Parallel()

	reader := &seqReader{states: []k8s.ReplicaStatus{{Desired: 0, Ready: 1}, {Desired: 0}}}
	sess, _ := confirmWithTracker(t, []*model.Workload{wl("legacy", 1)}, nil, []string{"legacy"}, reader)

	got := editContents(sess)
	if len(got) != 2 || !strings.Contains(got[0], "⏳ legacy stopping...") || !strings.Contains(got[1], "⏹ legacy stopped") {
		t.Fatalf("edits = %q, want stopping then stopped", got)
	}
}

func TestConfirm_ShowsTimeoutWithReason(t *testing.T) {
	t.Parallel()

	reader := &seqReader{states: []k8s.ReplicaStatus{{Desired: 1}}, issue: "ImagePullBackOff"}
	sess, _ := confirmWithTracker(t, []*model.Workload{wl("terraria", 0)}, []string{"terraria"}, nil, reader)

	got := editContents(sess)
	last := got[len(got)-1]
	if !strings.Contains(last, "⚠ terraria not ready after 10m (0/1 ready, ImagePullBackOff)") {
		t.Errorf("last edit = %q", last)
	}
	if len(got) != 2 {
		t.Errorf("edits = %d, want only the initial and the timeout edit", len(got))
	}
}

func TestConfirm_WithoutTrackerKeepsPlainResult(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, []*model.Workload{wl("terraria", 0)})
	token, _ := b.flows.create(&flow{userID: "u1", env: testCluster().Env, namespace: "labs", toStart: []string{"terraria"}})
	b.dispatch(component("u1", customID{kindConfirm, token, 0}.String()))
	b.watchers.Wait()

	if got := editContents(sess); len(got) != 1 || !strings.Contains(got[0], "🟢 terraria → start") {
		t.Errorf("edits = %q, want the plain result", got)
	}
}

func TestProgressText_UnknownWorkloadStaysNotFound(t *testing.T) {
	t.Parallel()

	res := frontend.ApplyResult{Applied: []frontend.Applied{{Action: frontend.ActionStart, Name: "gone", Found: false}}}
	got := progressText("u1", config.Cluster{Env: "stg", Name: "c"}, "labs", res, nil, 0)
	if !strings.Contains(got, "⚠ gone → start (not found)") {
		t.Errorf("got %q", got)
	}
}
