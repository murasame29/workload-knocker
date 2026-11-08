package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/model"
)

type posted struct {
	channel string
	ref     string
	msg     Message
}

// fakePoster records posts and edits of one platform.
type fakePoster struct {
	platform string
	mu       sync.Mutex
	posts    []posted
	edits    []posted
	postErr  error
}

func (f *fakePoster) Platform() string { return f.platform }

func (f *fakePoster) Post(_ context.Context, channel string, m Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return "", f.postErr
	}
	ref := f.platform + "-ref"
	f.posts = append(f.posts, posted{channel, ref, m})
	return ref, nil
}

func (f *fakePoster) Edit(_ context.Context, channel, ref string, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, posted{channel, ref, m})
	return nil
}

func registry(t *testing.T, discordCh, slackCh string) *config.Holder {
	t.Helper()
	cl := config.Cluster{
		Env: "stg", Name: "example-staging",
		Provider: config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
		Frontends: config.ClusterFrontends{
			Discord: &config.DiscordFrontend{Command: "knock", NotifyChannel: discordCh},
			Slack:   &config.SlackFrontend{SlashCommand: "/knock", NotifyChannel: slackCh},
		},
	}
	cfg := &config.Config{Controller: config.Controller{Namespace: "wk"}, Clusters: []config.Cluster{cl}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return config.NewHolder(cfg)
}

func newService(t *testing.T, discordCh, slackCh string) (*Service, *fakePoster, *fakePoster, *config.Holder) {
	t.Helper()
	reg := registry(t, discordCh, slackCh)
	s := New(reg, WithEditDelay(0))
	d, sl := &fakePoster{platform: config.FrontendDiscord}, &fakePoster{platform: config.FrontendSlack}
	s.AddPoster(d)
	s.AddPoster(sl)
	return s, d, sl, reg
}

func startEvent(workloads ...Workload) Event {
	return Event{Kind: KindStart, Env: "stg", ClusterName: "example-staging", Namespace: "games", Actor: "<@1>", StartedBy: "alice", Workloads: workloads}
}

const discordChannel = "123456789012345678"

func TestStarted_PostsOnceThenEditsWhenReady(t *testing.T) {
	t.Parallel()

	s, d, _, _ := newService(t, discordChannel, "")
	s.Started(context.Background(), startEvent(
		Workload{Namespace: "games", Name: "terraria", ReadyMessage: "Connect to {{.Name}}.example.com:7777 (started by {{.StartedBy}})"},
		Workload{Namespace: "games", Name: "minecraft"},
	))

	if len(d.posts) != 1 || d.posts[0].channel != discordChannel {
		t.Fatalf("posts = %+v, want one post to %s", d.posts, discordChannel)
	}
	first := d.posts[0].msg.Text
	for _, want := range []string{"<@1> requested start", "STG / example-staging / games", "⏳ terraria starting...", "⏳ minecraft starting..."} {
		if !strings.Contains(first, want) {
			t.Errorf("start message lacks %q:\n%s", want, first)
		}
	}

	s.Ready("stg", "games", "terraria")
	if len(d.posts) != 1 || len(d.edits) != 1 || d.edits[0].ref != d.posts[0].ref {
		t.Fatalf("posts=%d edits=%+v, want the same message edited once", len(d.posts), d.edits)
	}
	got := d.edits[0].msg.Text
	if !strings.Contains(got, "✅ terraria started — Connect to terraria.example.com:7777 (started by alice)") || !strings.Contains(got, "⏳ minecraft starting...") {
		t.Errorf("edit:\n%s", got)
	}

	s.Ready("stg", "games", "minecraft")
	if last := d.edits[len(d.edits)-1].msg.Text; !strings.Contains(last, "✅ minecraft started") || strings.Contains(last, "⏳") {
		t.Errorf("final edit:\n%s", last)
	}

	s.Ready("stg", "games", "minecraft")
	if len(d.edits) != 2 {
		t.Errorf("a repeated ready must not edit again, edits = %d", len(d.edits))
	}
}

func TestStarted_BrokenTemplateFallsBackToPlainLine(t *testing.T) {
	t.Parallel()

	s, d, _, _ := newService(t, discordChannel, "")
	s.Started(context.Background(), startEvent(Workload{Namespace: "games", Name: "terraria", ReadyMessage: "{{.Missing}}"}))
	s.Ready("stg", "games", "terraria")

	if len(d.edits) != 1 || !strings.Contains(d.edits[0].msg.Text, "✅ terraria started") || strings.Contains(d.edits[0].msg.Text, "—") {
		t.Errorf("edits = %+v, want the plain started line", d.edits)
	}
}

func TestNotify_NothingPostedWithoutChannel(t *testing.T) {
	t.Parallel()

	s, d, sl, _ := newService(t, "", "")
	if s.Enabled("stg") {
		t.Error("Enabled with no channel")
	}
	s.Started(context.Background(), startEvent(Workload{Namespace: "games", Name: "terraria"}))
	s.Ready("stg", "games", "terraria")
	s.Stopped(context.Background(), Event{Kind: KindStop, Env: "stg", Workloads: []Workload{{Namespace: "games", Name: "terraria"}}})
	s.StopReminder(context.Background(), model.Workload{Env: "stg", Namespace: "games", Name: "terraria"}, time.Now())
	if len(d.posts)+len(d.edits)+len(sl.posts)+len(sl.edits) != 0 {
		t.Errorf("unexpected traffic: discord %+v slack %+v", d, sl)
	}
}

func TestNotify_PostsToEveryConfiguredPlatform(t *testing.T) {
	t.Parallel()

	s, d, sl, _ := newService(t, discordChannel, "C0123ABCD")
	s.Started(context.Background(), startEvent(Workload{Namespace: "games", Name: "terraria"}))
	s.Ready("stg", "games", "terraria")

	if len(d.posts) != 1 || len(sl.posts) != 1 || sl.posts[0].channel != "C0123ABCD" {
		t.Fatalf("discord %+v slack %+v", d.posts, sl.posts)
	}
	if len(d.edits) != 1 || len(sl.edits) != 1 {
		t.Errorf("edits discord=%d slack=%d, want 1 each", len(d.edits), len(sl.edits))
	}
}

func TestNotify_FailedPostIsNotFatalAndNotEdited(t *testing.T) {
	t.Parallel()

	s, d, _, _ := newService(t, discordChannel, "")
	d.postErr = errors.New("missing permissions")
	s.Started(context.Background(), startEvent(Workload{Namespace: "games", Name: "terraria"}))
	s.Ready("stg", "games", "terraria")
	if len(d.posts) != 0 || len(d.edits) != 0 {
		t.Errorf("posts=%d edits=%d, want none after a failed post", len(d.posts), len(d.edits))
	}
}

func TestStopped_OneMessagePerOperation(t *testing.T) {
	t.Parallel()

	s, d, _, _ := newService(t, discordChannel, "")
	s.Stopped(context.Background(), Event{
		Kind: KindStop, Env: "stg", ClusterName: "example-staging", Namespace: "games", Actor: "<@1>",
		Workloads: []Workload{{Namespace: "games", Name: "b"}, {Namespace: "games", Name: "a"}},
	})
	if len(d.posts) != 1 || len(d.edits) != 0 {
		t.Fatalf("posts=%d edits=%d", len(d.posts), len(d.edits))
	}
	text := d.posts[0].msg.Text
	if !strings.Contains(text, "<@1> stopped workloads (STG / example-staging / games)") || strings.Index(text, "⏹ a") > strings.Index(text, "⏹ b") {
		t.Errorf("stop message:\n%s", text)
	}

	s.Stopped(context.Background(), Event{
		Kind: KindStopAll, Env: "stg", ClusterName: "example-staging", Actor: "nightly-stop",
		Workloads: []Workload{{Namespace: "games", Name: "a"}, {Namespace: "web", Name: "c"}},
	})
	all := d.posts[1].msg.Text
	if !strings.Contains(all, "nightly-stop stopped") || !strings.Contains(all, "games/a") || !strings.Contains(all, "web/c") {
		t.Errorf("stop_all message:\n%s", all)
	}
}

func TestStopped_CancelsPendingReadyEdit(t *testing.T) {
	t.Parallel()

	s, d, _, _ := newService(t, discordChannel, "")
	s.Started(context.Background(), startEvent(Workload{Namespace: "games", Name: "terraria"}))
	s.Stopped(context.Background(), Event{Kind: KindStop, Env: "stg", Workloads: []Workload{{Namespace: "games", Name: "terraria"}}})
	s.Ready("stg", "games", "terraria")
	if len(d.edits) != 0 {
		t.Errorf("edits = %d, want none after the workload was stopped", len(d.edits))
	}
}

func TestStopReminder_PostsExtendButton(t *testing.T) {
	t.Parallel()

	s, d, _, _ := newService(t, discordChannel, "")
	jst := time.FixedZone("JST", 9*3600)
	s.StopReminder(context.Background(), model.Workload{Env: "stg", Namespace: "games", Name: "terraria"}, time.Date(2026, 1, 1, 22, 0, 0, 0, jst))

	if len(d.posts) != 1 {
		t.Fatalf("posts = %d", len(d.posts))
	}
	m := d.posts[0].msg
	if !strings.Contains(m.Text, "terraria stops at 22:00 JST") || len(m.Buttons) != 1 || m.Buttons[0].Label != "Extend 1h" {
		t.Fatalf("message = %+v", m)
	}
	ref, err := DecodeExtend(m.Buttons[0].Payload)
	if err != nil || ref != (WorkloadRef{Env: "stg", Namespace: "games", Name: "terraria"}) {
		t.Errorf("payload decodes to %+v, %v", ref, err)
	}
}

func TestNotify_HotReloadedChannelApplies(t *testing.T) {
	t.Parallel()

	s, d, _, reg := newService(t, "", "")
	if s.Enabled("stg") {
		t.Fatal("enabled before reload")
	}
	reg.Store(registry(t, discordChannel, "").Load())
	if !s.Enabled("stg") {
		t.Fatal("a reloaded notify channel must apply without restart")
	}
	s.Stopped(context.Background(), Event{Kind: KindStop, Env: "stg", Actor: "x", Workloads: []Workload{{Namespace: "games", Name: "a"}}})
	if len(d.posts) != 1 {
		t.Errorf("posts = %d, want 1", len(d.posts))
	}
	reg.Store(registry(t, "", "").Load())
	if s.Enabled("stg") {
		t.Error("removing the channel must stop posting")
	}
}

func TestStarted_CoalescesReadyEdits(t *testing.T) {
	t.Parallel()

	reg := registry(t, discordChannel, "")
	s := New(reg, WithEditDelay(20*time.Millisecond))
	d := &fakePoster{platform: config.FrontendDiscord}
	s.AddPoster(d)
	s.Started(context.Background(), startEvent(Workload{Namespace: "games", Name: "a"}, Workload{Namespace: "games", Name: "b"}))
	s.Ready("stg", "games", "a")
	s.Ready("stg", "games", "b")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		n := len(d.edits)
		d.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.edits) != 1 || strings.Contains(d.edits[0].msg.Text, "⏳") {
		t.Errorf("edits = %+v, want one edit with both ready", d.edits)
	}
}

func TestDecodeExtend_Invalid(t *testing.T) {
	t.Parallel()

	for _, p := range []string{"", "x", `{"e":"stg"}`, `{"e":"","n":"a","w":"b"}`} {
		if _, err := DecodeExtend(p); err == nil {
			t.Errorf("DecodeExtend(%q): want error", p)
		}
	}
}

func TestExtendReply(t *testing.T) {
	t.Parallel()

	ref := WorkloadRef{Name: "terraria"}
	jst := time.FixedZone("JST", 9*3600)
	cases := []struct {
		err  error
		at   time.Time
		want string
	}{
		{ErrExtendUnavailable, time.Time{}, "not available yet"},
		{errors.New("not authorized"), time.Time{}, "not authorized"},
		{nil, time.Date(2026, 1, 1, 23, 0, 0, 0, jst), "now stops at 23:00 JST"},
		{nil, time.Time{}, "by 1h"},
	}
	for _, c := range cases {
		if got := ExtendReply(ref, c.at, c.err); !strings.Contains(got, c.want) {
			t.Errorf("ExtendReply(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	if _, err := UnavailableExtender().Extend(context.Background(), ExtendRequest{}); !errors.Is(err, ErrExtendUnavailable) {
		t.Errorf("default extender error = %v", err)
	}
}
