package slackbot

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/notify"
)

type chatCall struct {
	channel string
	ts      string
	values  map[string][]string
}

// fakeChat records chat.postMessage and chat.update calls.
type fakeChat struct {
	mu    sync.Mutex
	posts []chatCall
	edits []chatCall
}

func (f *fakeChat) render(options []slack.MsgOption) map[string][]string {
	_, values, err := slack.UnsafeApplyMsgOptions("token", "C0", "https://slack.com/api/", options...)
	if err != nil {
		panic(err)
	}
	return values
}

func (f *fakeChat) PostMessageContext(_ context.Context, channel string, options ...slack.MsgOption) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, chatCall{channel: channel, values: f.render(options)})
	return channel, "1700000000.000100", nil
}

func (f *fakeChat) UpdateMessageContext(_ context.Context, channel, ts string, options ...slack.MsgOption) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, chatCall{channel: channel, ts: ts, values: f.render(options)})
	return channel, ts, "", nil
}

type slackRegistry struct{ cl config.Cluster }

func (r slackRegistry) ByEnv(string) (config.Cluster, bool) { return r.cl, true }

const slackChannel = "C0123ABCD"

func TestPoster_PostThenEditUsesReturnedTimestamp(t *testing.T) {
	t.Parallel()

	chat := &fakeChat{}
	cl := config.Cluster{Env: "stg", Name: "example-staging", Frontends: config.ClusterFrontends{Slack: &config.SlackFrontend{SlashCommand: "/k", NotifyChannel: slackChannel}}}
	n := notify.New(slackRegistry{cl}, notify.WithEditDelay(0))
	n.AddPoster(poster{api: chat})

	n.Started(context.Background(), notify.Event{
		Kind: notify.KindStart, Env: "stg", ClusterName: "example-staging", Namespace: "games", Actor: "<@U1>",
		Workloads: []notify.Workload{{Namespace: "games", Name: "terraria", ReadyMessage: "Connect to {{.Name}}"}},
	})
	n.Ready("stg", "games", "terraria")

	if len(chat.posts) != 1 || chat.posts[0].channel != slackChannel {
		t.Fatalf("posts = %+v", chat.posts)
	}
	if len(chat.edits) != 1 || chat.edits[0].ts != "1700000000.000100" || chat.edits[0].channel != slackChannel {
		t.Fatalf("edits = %+v, want chat.update with the timestamp chat.postMessage returned", chat.edits)
	}
	if text := strings.Join(chat.edits[0].values["text"], ""); !strings.Contains(text, "✅ terraria started — Connect to terraria") {
		t.Errorf("edit text = %q", text)
	}
	if text := strings.Join(chat.posts[0].values["text"], ""); !strings.Contains(text, "<@U1> requested start") {
		t.Errorf("post text = %q (mentions must stay unescaped)", text)
	}
}

func TestMessageBlocks_ExtendButton(t *testing.T) {
	t.Parallel()

	payload := notify.EncodeExtend(notify.WorkloadRef{Env: "stg", Namespace: "games", Name: "terraria"})
	blocks := messageBlocks(notify.Message{Text: "⏰ terraria stops at 22:00 JST", Buttons: []notify.Button{{Label: "Extend 1h", Payload: payload}}})
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want a section and an action row", len(blocks))
	}
	btn := blocks[1].(*slack.ActionBlock).Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if btn.ActionID != actionExtend || btn.Value != payload || btn.Text.Text != "Extend 1h" {
		t.Errorf("button = %+v", btn)
	}
	if got := messageBlocks(notify.Message{Text: "plain"}); len(got) != 1 {
		t.Errorf("a message without buttons has only the section, got %d blocks", len(got))
	}
}

func TestPoster_LongTextIsTruncated(t *testing.T) {
	t.Parallel()

	chat := &fakeChat{}
	if _, err := (poster{api: chat}).Post(context.Background(), slackChannel, notify.Message{Text: strings.Repeat("あ", 4000)}); err != nil {
		t.Fatal(err)
	}
	if got := []rune(strings.Join(chat.posts[0].values["text"], "")); len(got) != slackSectionTextMaxLen {
		t.Errorf("text has %d runes, want %d", len(got), slackSectionTextMaxLen)
	}
}

func TestNotify_NoChannelNoCalls(t *testing.T) {
	t.Parallel()

	chat := &fakeChat{}
	cl := config.Cluster{Env: "stg", Frontends: config.ClusterFrontends{Slack: &config.SlackFrontend{SlashCommand: "/k"}}}
	n := notify.New(slackRegistry{cl}, notify.WithEditDelay(0))
	n.AddPoster(poster{api: chat})
	n.Started(context.Background(), notify.Event{Env: "stg", Workloads: []notify.Workload{{Namespace: "a", Name: "b"}}})
	n.Ready("stg", "a", "b")
	n.Stopped(context.Background(), notify.Event{Env: "stg", Workloads: []notify.Workload{{Namespace: "a", Name: "b"}}})
	if len(chat.posts)+len(chat.edits) != 0 {
		t.Errorf("posts=%d edits=%d, want none", len(chat.posts), len(chat.edits))
	}
}
