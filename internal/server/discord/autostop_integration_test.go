package discordbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/autostop"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/notify"
)

// stopAtStore is a fakeStore that also knows clusters and moves stop times.
type stopAtStore struct {
	*fakeStore
	setCalls []time.Time
}

func (s *stopAtStore) Clusters() []model.ClusterRef {
	return []model.ClusterRef{{Env: "stg", Name: "example-staging"}}
}

func (s *stopAtStore) SetStopAt(_, _, name string, at time.Time) bool {
	for _, w := range s.workloads {
		if w.Name == name {
			w.StopAt = at
			s.setCalls = append(s.setCalls, at)
			return true
		}
	}
	return false
}

type auditSink struct{ recs []audit.Record }

func (a *auditSink) Record(_ context.Context, r audit.Record) { a.recs = append(a.recs, r) }

func TestAutostopReminderReachesNotifyChannelAndExtendMovesStopAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 21, 55, 0, 0, time.FixedZone("JST", 9*3600))
	stopAt := now.Add(5 * time.Minute)
	store := &stopAtStore{fakeStore: &fakeStore{workloads: []*model.Workload{{Name: "terraria", Namespace: "labs", Replicas: 1, StopAt: stopAt}}}}

	cl := testCluster()
	cl.Frontends.Discord.NotifyChannel = notifyChannel
	reg := &config.Config{Controller: config.Controller{Namespace: "wk"}, Clusters: []config.Cluster{cl}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	holder := config.NewHolder(reg)
	n := notify.New(holder, notify.WithEditDelay(0))
	rec := &auditSink{}
	svc := frontend.NewService(store, metrics.New(), time.Second,
		frontend.WithAuthorizer(authz.NewHolder(nil)), frontend.WithMode(mode.Full), frontend.WithNotifier(n),
		frontend.WithClusters(holder), frontend.WithClock(func() time.Time { return now }), frontend.WithRecorder(rec))
	sess := &fakeSession{}
	b := newBot(sess, svc, holder, "guild-1")
	n.AddPoster(b.Poster())

	autostop.New(store, metrics.New(), 10*time.Minute, autostop.WithReminder(n)).Tick(context.Background(), now)

	if len(sess.sent) != 1 || sess.sent[0].channel != notifyChannel || !strings.Contains(sess.sent[0].msg.Content, "terraria stops at 22:00 JST") {
		t.Fatalf("sent = %+v, want the reminder in the notify channel", sess.sent)
	}

	b.dispatch(component("u1", extendButton(t, sess)))

	want := stopAt.Add(time.Hour)
	if len(store.setCalls) != 1 || !store.setCalls[0].Equal(want) {
		t.Fatalf("SetStopAt calls = %v, want %v", store.setCalls, want)
	}
	if c := sess.responses[len(sess.responses)-1].Data.Content; !strings.Contains(c, "now stops at 23:00 JST") {
		t.Errorf("reply = %q", c)
	}
	if len(rec.recs) != 1 || rec.recs[0].Event != audit.WorkloadExtend || rec.recs[0].Outcome != audit.Success || !rec.recs[0].StopAt.Equal(want) {
		t.Errorf("audit = %+v, want one successful workload.extend", rec.recs)
	}
}

func TestExtendButton_DeniedByPolicyIsRefused(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 21, 55, 0, 0, time.UTC)
	store := &stopAtStore{fakeStore: &fakeStore{workloads: []*model.Workload{{Name: "terraria", Namespace: "labs", Replicas: 1, StopAt: now.Add(time.Minute)}}}}
	cl := testCluster()
	cl.Frontends.Discord.NotifyChannel = notifyChannel
	reg := &config.Config{Controller: config.Controller{Namespace: "wk"}, Clusters: []config.Cluster{cl}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	holder := config.NewHolder(reg)
	n := notify.New(holder, notify.WithEditDelay(0))
	policy, err := authz.Parse([]byte("version: 1\ndefault: deny\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc := frontend.NewService(store, metrics.New(), time.Second,
		frontend.WithAuthorizer(authz.NewHolder(policy)), frontend.WithNotifier(n), frontend.WithClusters(holder))
	sess := &fakeSession{}
	b := newBot(sess, svc, holder, "guild-1")
	n.AddPoster(b.Poster())
	n.StopReminder(context.Background(), model.Workload{Env: "stg", Namespace: "labs", Name: "terraria"}, now)

	b.dispatch(component("u1", extendButton(t, sess)))

	if len(store.setCalls) != 0 {
		t.Errorf("a denied user must not move the stop time: %v", store.setCalls)
	}
	if c := sess.responses[len(sess.responses)-1].Data.Content; !strings.Contains(c, "not authorized") {
		t.Errorf("reply = %q", c)
	}
}
