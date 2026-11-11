package discordbot

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

var discordNow = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

func newDurationBot(t *testing.T, ws []*model.Workload) (*Bot, *fakeSession, *fakeStore) {
	t.Helper()
	reg := &config.Config{Controller: config.Controller{Namespace: "wk"}, Clusters: []config.Cluster{testCluster()}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{workloads: ws}
	sess := &fakeSession{}
	svc := frontend.NewService(store, metrics.New(), time.Second,
		frontend.WithAuthorizer(authz.NewHolder(nil)),
		frontend.WithClock(func() time.Time { return discordNow }),
		frontend.WithRunDefaults(runlimit.Defaults{Default: 4 * time.Hour, Max: 8 * time.Hour}))
	b := newBot(sess, svc, config.NewHolder(reg), "guild-1")
	b.noModal = true
	return b, sess, store
}

// previewFlow drives the menu up to the plan preview that starts "batch" and "shop-app".
func previewFlow(t *testing.T, b *Bot, sess *fakeSession) (token string, preview *discordgo.InteractionResponseData) {
	t.Helper()
	b.dispatch(command("u1", "knock-stg"))
	_, r := lastEdit(t, sess)
	nsMenu := r[0].Components[0].(discordgo.SelectMenu)
	b.dispatch(component("u1", nsMenu.CustomID, "labs"))
	wlRows := lastUpdate(t, sess).Components
	wlMenu := wlRows[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	b.dispatch(component("u1", wlMenu.CustomID, "0", "1"))
	for _, c := range lastUpdate(t, sess).Components[1].(discordgo.ActionsRow).Components {
		if btn := c.(discordgo.Button); btn.Label == "Apply" {
			b.dispatch(component("u1", btn.CustomID))
		}
	}
	preview = lastUpdate(t, sess)
	id, err := parseCustomID(nsMenu.CustomID)
	if err != nil {
		t.Fatal(err)
	}
	return id.Token, preview
}

func buttonID(t *testing.T, d *discordgo.InteractionResponseData, label string) string {
	t.Helper()
	for _, row := range d.Components {
		for _, c := range row.(discordgo.ActionsRow).Components {
			if btn, ok := c.(discordgo.Button); ok && btn.Label == label {
				return btn.CustomID
			}
		}
	}
	t.Fatalf("no %q button in %+v", label, d.Components)
	return ""
}

func modalSubmit(user, token string, inputs ...string) *discordgo.Interaction {
	var rows []discordgo.MessageComponent
	for i, v := range inputs {
		rows = append(rows, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			&discordgo.TextInput{CustomID: string(rune('0' + i)), Value: v},
		}})
	}
	return &discordgo.Interaction{
		Type:   discordgo.InteractionModalSubmit,
		Member: member(user),
		Data:   discordgo.ModalSubmitInteractionData{CustomID: customID{kindDurationModal, token, 0}.String(), Components: rows},
	}
}

func lastResponse(s *fakeSession) *discordgo.InteractionResponse {
	return s.responses[len(s.responses)-1]
}

func TestPreview_ShowsStopTimeAndChangeDurationButton(t *testing.T) {
	t.Parallel()

	b, sess, _ := newDurationBot(t, []*model.Workload{wl("batch", 0), wl("shop-app", 0)})
	_, preview := previewFlow(t, b, sess)

	want := "<t:" + itoa(discordNow.Add(4*time.Hour).Unix()) + ":f>"
	if !strings.Contains(preview.Content, "auto-stop in 4h") || !strings.Contains(preview.Content, want) {
		t.Errorf("preview lacks the default stop time:\n%s", preview.Content)
	}
	buttonID(t, preview, "Change duration")
}

func TestDurationModal_OpensPrefilledWithCurrentHours(t *testing.T) {
	t.Parallel()

	b, sess, _ := newDurationBot(t, []*model.Workload{wl("batch", 0), wl("shop-app", 0)})
	_, preview := previewFlow(t, b, sess)
	b.dispatch(component("u1", buttonID(t, preview, "Change duration")))

	resp := lastResponse(sess)
	if resp.Type != discordgo.InteractionResponseModal || len(resp.Data.Components) != 2 {
		t.Fatalf("response = %+v", resp)
	}
	in := resp.Data.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.TextInput)
	if in.Value != "4" || in.Required || in.Label != "batch" || !strings.Contains(in.Placeholder, "max 8h") {
		t.Errorf("input = %+v", in)
	}
}

func TestDurationModal_ValidInputRefreshesPreviewAndApplies(t *testing.T) {
	t.Parallel()

	b, sess, store := newDurationBot(t, []*model.Workload{wl("batch", 0), wl("shop-app", 0)})
	token, preview := previewFlow(t, b, sess)
	confirm := buttonID(t, preview, "Confirm")

	b.dispatch(modalSubmit("u1", token, "1.5", ""))
	resp := lastResponse(sess)
	if resp.Type != discordgo.InteractionResponseUpdateMessage {
		t.Fatalf("response type = %v, want UpdateMessage", resp.Type)
	}
	c := resp.Data.Content
	if !strings.Contains(c, "batch: auto-stop in 1.5h") || !strings.Contains(c, "shop-app: auto-stop in 4h") {
		t.Errorf("preview after change:\n%s", c)
	}

	b.dispatch(component("u1", confirm))
	got := *sess.edits[len(sess.edits)-1].Content
	if !strings.Contains(got, "batch: auto-stop at <t:"+itoa(discordNow.Add(90*time.Minute).Unix())+":f>") {
		t.Errorf("result lacks the stop time of batch:\n%s", got)
	}
	if len(store.calls) != 2 {
		t.Errorf("store calls = %v", store.calls)
	}
}

func TestDurationModal_InvalidInputRepliesWithReasonAndRetryReopensPrefilled(t *testing.T) {
	t.Parallel()

	b, sess, _ := newDurationBot(t, []*model.Workload{wl("batch", 0), wl("shop-app", 0)})
	token, preview := previewFlow(t, b, sess)

	b.dispatch(modalSubmit("u1", token, "banana", "9"))
	resp := lastResponse(sess)
	if resp.Type != discordgo.InteractionResponseChannelMessageWithSource || resp.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("invalid input must get an ephemeral reply: %+v", resp)
	}
	for _, want := range []string{"batch:", "not a number of hours", "shop-app:", "at most 8h"} {
		if !strings.Contains(resp.Data.Content, want) {
			t.Errorf("reply lacks %q:\n%s", want, resp.Data.Content)
		}
	}

	b.dispatch(component("u1", buttonID(t, resp.Data, "Try again")))
	modal := lastResponse(sess)
	if modal.Type != discordgo.InteractionResponseModal {
		t.Fatalf("Try again must reopen the modal: %+v", modal)
	}
	vals := []string{
		modal.Data.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.TextInput).Value,
		modal.Data.Components[1].(discordgo.ActionsRow).Components[0].(discordgo.TextInput).Value,
	}
	if vals[0] != "banana" || vals[1] != "9" {
		t.Errorf("prefill = %v, want the previous input", vals)
	}

	// The rejected input changed nothing: the original preview still confirms with defaults.
	b.dispatch(component("u1", buttonID(t, preview, "Confirm")))
	if got := *sess.edits[len(sess.edits)-1].Content; !strings.Contains(got, "auto-stop at") {
		t.Errorf("result = %q", got)
	}
}

func TestDurationModal_ForeignUserAndBelowMinimum(t *testing.T) {
	t.Parallel()

	b, sess, _ := newDurationBot(t, []*model.Workload{wl("batch", 0)})
	token, _ := previewFlow(t, b, sess)

	b.dispatch(modalSubmit("intruder", token, "2"))
	if got := lastResponse(sess).Data.Content; got != msgForeign {
		t.Errorf("foreign modal = %q", got)
	}
	b.dispatch(modalSubmit("u1", token, "0.1"))
	if got := lastResponse(sess).Data.Content; !strings.Contains(got, "at least 0.25h") {
		t.Errorf("below minimum = %q", got)
	}
}

func TestDurationModal_CapsInputsAtFive(t *testing.T) {
	t.Parallel()

	var starts []frontend.Schedule
	for i := range 7 {
		starts = append(starts, frontend.Schedule{Name: "svc-" + itoa(int64(i))})
	}
	resp := durationModal("tok", starts, nil)
	if len(resp.Data.Components) != maxDurationInputs {
		t.Errorf("inputs = %d, want %d", len(resp.Data.Components), maxDurationInputs)
	}
	v := previewView("tok", testCluster(), []section{{namespace: "labs", changes: nil, rejected: nil, starts: starts}})
	if !strings.Contains(v.Content, "first 5 starts") {
		t.Errorf("preview lacks the cap note:\n%s", v.Content)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
