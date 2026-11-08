package discordbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/notify"
	"github.com/murasame29/workload-knocker/internal/rollout"
)

const notifyChannel = "123456789012345678"

type extendCall struct{ req notify.ExtendRequest }

type fakeExtender struct {
	calls []extendCall
	at    time.Time
	err   error
}

func (f *fakeExtender) Extend(_ context.Context, req notify.ExtendRequest) (time.Time, error) {
	f.calls = append(f.calls, extendCall{req})
	return f.at, f.err
}

// notifyBot builds a Bot whose cluster has a notify channel and whose rollout reports ready at once.
func notifyBot(t *testing.T, ws []*model.Workload, opts ...frontend.Option) (*Bot, *fakeSession, *notify.Service) {
	t.Helper()
	cl := testCluster()
	cl.Frontends.Discord.NotifyChannel = notifyChannel
	reg := &config.Config{Controller: config.Controller{Namespace: "wk"}, Clusters: []config.Cluster{cl}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	holder := config.NewHolder(reg)
	n := notify.New(holder, notify.WithEditDelay(0))
	reader := &seqReader{states: []k8s.ReplicaStatus{{Desired: 1, Ready: 1}}}
	tracker := rollout.NewTracker(oneCluster{reader}, 5*time.Second, 10*time.Minute,
		rollout.WithClock(&stepClock{now: time.Unix(0, 0)}),
		rollout.WithOnStarted(func(tg rollout.Target) { n.Ready(tg.Env, tg.Namespace, tg.Name) }))
	all := append([]frontend.Option{frontend.WithAuthorizer(authz.NewHolder(nil)), frontend.WithMode(mode.Full), frontend.WithRollout(tracker), frontend.WithNotifier(n)}, opts...)
	svc := frontend.NewService(&fakeStore{workloads: ws}, metrics.New(), time.Second, all...)
	sess := &fakeSession{}
	b := newBot(sess, svc, holder, "guild-1")
	n.AddPoster(b.Poster())
	return b, sess, n
}

func TestConfirm_PostsToNotifyChannelThenEditsWhenReady(t *testing.T) {
	t.Parallel()

	ws := []*model.Workload{{Name: "terraria", Namespace: "labs", ReadyMessage: "Connect to {{.Name}}.example.com:7777"}}
	b, sess, _ := notifyBot(t, ws)
	token, err := b.flows.create(&flow{userID: "u1", env: "stg", namespace: "labs", toStart: []string{"terraria"}})
	if err != nil {
		t.Fatal(err)
	}
	b.dispatch(component("u1", customID{kindConfirm, token, 0}.String()))
	b.watchers.Wait()

	if len(sess.sent) != 1 || sess.sent[0].channel != notifyChannel {
		t.Fatalf("sent = %+v, want one post to the notify channel", sess.sent)
	}
	if c := sess.sent[0].msg.Content; !strings.Contains(c, "<@u1> requested start") || !strings.Contains(c, "⏳ terraria starting...") {
		t.Errorf("start post: %q", c)
	}
	if sess.sent[0].msg.AllowedMentions == nil || len(sess.sent[0].msg.AllowedMentions.Parse) != 0 {
		t.Error("the public post must not ping anyone")
	}
	if len(sess.edited) != 1 || sess.edited[0].Channel != notifyChannel || sess.edited[0].ID != "msg-1" {
		t.Fatalf("edited = %+v, want the posted message edited once", sess.edited)
	}
	if c := *sess.edited[0].Content; !strings.Contains(c, "✅ terraria started — Connect to terraria.example.com:7777") {
		t.Errorf("ready edit: %q", c)
	}
	if got := editContents(sess); !strings.Contains(got[len(got)-1], "✅ terraria started — Connect to terraria.example.com:7777") {
		t.Errorf("the starter's progress message lacks the ready message: %q", got)
	}
}

func TestConfirm_NothingPostedWithoutNotifyChannel(t *testing.T) {
	t.Parallel()

	reader := &seqReader{states: []k8s.ReplicaStatus{{Desired: 1, Ready: 1}}}
	sess, _ := confirmWithTracker(t, []*model.Workload{wl("terraria", 0)}, []string{"terraria"}, nil, reader)
	if len(sess.sent) != 0 || len(sess.edited) != 0 {
		t.Errorf("sent=%d edited=%d, want no channel traffic", len(sess.sent), len(sess.edited))
	}
}

func extendButton(t *testing.T, sess *fakeSession) string {
	t.Helper()
	row := sess.sent[0].msg.Components[0].(discordgo.ActionsRow)
	btn := row.Components[0].(discordgo.Button)
	if btn.Label != "Extend 1h" || len(btn.CustomID) >= 100 {
		t.Fatalf("button = %+v", btn)
	}
	return btn.CustomID
}

func TestStopReminder_ExtendButtonDefaultsToUnavailable(t *testing.T) {
	t.Parallel()

	b, sess, n := notifyBot(t, nil)
	n.StopReminder(context.Background(), model.Workload{Env: "stg", Namespace: "labs", Name: "terraria"}, time.Now())
	b.dispatch(component("u1", extendButton(t, sess)))

	r := sess.responses[len(sess.responses)-1]
	if !strings.Contains(r.Data.Content, "not available yet") || r.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Errorf("response = %+v", r.Data)
	}
}

func TestStopReminder_ExtendButtonCallsExtender(t *testing.T) {
	t.Parallel()

	ext := &fakeExtender{at: time.Date(2026, 1, 1, 23, 0, 0, 0, time.FixedZone("JST", 9*3600))}
	b, sess, n := notifyBot(t, nil, frontend.WithExtender(ext))
	n.StopReminder(context.Background(), model.Workload{Env: "stg", Namespace: "labs", Name: "terraria"}, time.Now())
	b.dispatch(component("u1", extendButton(t, sess)))

	if len(ext.calls) != 1 {
		t.Fatalf("calls = %d", len(ext.calls))
	}
	got := ext.calls[0].req
	if got.Ref != (notify.WorkloadRef{Env: "stg", Namespace: "labs", Name: "terraria"}) || got.By != time.Hour || got.Subject.User != "u1" || got.Subject.Platform != authz.PlatformDiscord {
		t.Errorf("request = %+v", got)
	}
	if c := sess.responses[len(sess.responses)-1].Data.Content; !strings.Contains(c, "now stops at 23:00 JST") {
		t.Errorf("reply = %q", c)
	}
}

func TestExtendButton_UnknownTokenIsExpired(t *testing.T) {
	t.Parallel()

	b, sess, _ := notifyBot(t, nil)
	b.dispatch(component("u1", customID{kindExtend, "deadbeefdead", 0}.String()))
	if c := sess.responses[len(sess.responses)-1].Data.Content; c != msgExpired {
		t.Errorf("reply = %q, want %q", c, msgExpired)
	}
}

func TestPoster_ButtonsAreClearedOnEdit(t *testing.T) {
	t.Parallel()

	b, sess, _ := notifyBot(t, nil)
	p := b.poster
	if err := p.Edit(context.Background(), notifyChannel, "m1", notify.Message{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(sess.edited) != 1 || *sess.edited[0].Components == nil || len(*sess.edited[0].Components) != 0 {
		t.Errorf("edit components must be a non-nil empty slice, got %+v", sess.edited)
	}
}
