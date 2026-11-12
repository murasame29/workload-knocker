package discordbot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/policy/mode"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

const starterID = "123456789012345678"

// statusStore is an in-memory store whose Stop and SetStopAt change the state; it counts Reconcile calls.
type statusStore struct {
	model.WorkloadStore
	mu         sync.Mutex
	workloads  []*model.Workload
	reconciles int
	stops      []string
	stopAts    map[string]time.Time
}

func (s *statusStore) Namespaces(string) []string { return []string{"labs"} }
func (s *statusStore) WorkloadsByNamespace(string, string) []*model.Workload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.Workload, 0, len(s.workloads))
	for _, w := range s.workloads {
		c := *w
		out = append(out, &c)
	}
	return out
}

func (s *statusStore) Reconcile(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciles++
	return nil
}

func (s *statusStore) Stop(_, _, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workloads {
		if w.Name == name {
			w.Replicas, w.StopAt, w.StartedBy = 0, time.Time{}, ""
			s.stops = append(s.stops, name)
			return nil
		}
	}
	return model.ErrWorkloadNotFound
}

func (s *statusStore) SetStopAt(_, _, name string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workloads {
		if w.Name == name && w.Replicas > 0 {
			w.StopAt = at
			if s.stopAts == nil {
				s.stopAts = map[string]time.Time{}
			}
			s.stopAts[name] = at
			return true
		}
	}
	return false
}

func newStatusBot(t *testing.T, ws []*model.Workload, policy string, m mode.Mode) (*Bot, *fakeSession, *statusStore, *auditSink) {
	t.Helper()
	reg := &config.Config{Controller: config.Controller{Namespace: "wk"}, Clusters: []config.Cluster{testCluster()}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	var p *authz.Policy
	if policy != "" {
		var err error
		if p, err = authz.Parse([]byte(policy)); err != nil {
			t.Fatal(err)
		}
	}
	store := &statusStore{workloads: ws}
	rec := &auditSink{}
	holder := config.NewHolder(reg)
	svc := frontend.NewService(store, metrics.New(), time.Second,
		frontend.WithAuthorizer(authz.NewHolder(p)), frontend.WithMode(m), frontend.WithRecorder(rec), frontend.WithClusters(holder))
	sess := &fakeSession{}
	return newBot(sess, svc, holder, "guild-1"), sess, store, rec
}

func statusCommand(user string, roles ...string) *discordgo.Interaction {
	i := commandWithRoles(user, "knock-stg", roles...)
	i.Data = discordgo.ApplicationCommandInteractionData{
		Name:    "knock-stg",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: subcommandStatus, Type: discordgo.ApplicationCommandOptionSubCommand}},
	}
	return i
}

func runningWL(name string, stopAt time.Time) *model.Workload {
	return &model.Workload{
		Env: "stg", Name: name, Namespace: "labs", Kind: "Deployment", Replicas: 1,
		StartedBy: starterID, StartedAt: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC), StopAt: stopAt,
	}
}

// statusButtons returns the buttons of the message by label.
func statusButtons(d *discordgo.InteractionResponseData) map[string]discordgo.Button {
	out := map[string]discordgo.Button{}
	for _, c := range d.Components {
		for _, b := range c.(discordgo.ActionsRow).Components {
			btn := b.(discordgo.Button)
			out[btn.Label] = btn
		}
	}
	return out
}

func TestStatus_ListsWorkloadsEphemerallyWithoutClusterCall(t *testing.T) {
	t.Parallel()

	stopAt := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	b, sess, store, _ := newStatusBot(t, []*model.Workload{runningWL("shop-api", stopAt), wl("shop-app", 0), runningWL("batch", time.Time{}), wl("idle", 0)}, "", mode.Full)

	b.dispatch(statusCommand("u1"))

	if len(sess.responses) != 1 || store.reconciles != 0 {
		t.Fatalf("responses=%d reconciles=%d, want one direct answer and no cluster call", len(sess.responses), store.reconciles)
	}
	r := sess.responses[0]
	if r.Type != discordgo.InteractionResponseChannelMessageWithSource || r.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("response must be an ephemeral message: %+v", r)
	}
	for _, want := range []string{
		"**STG / example-staging** status", "**labs**",
		"🟡 shop (api, app) 1/2 running · by <@" + starterID + "> · started <t:" + fmt.Sprint(time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC).Unix()) + ":R> · stops <t:" + fmt.Sprint(stopAt.Unix()) + ":R>",
		"🟢 batch (1 pod) · by <@" + starterID + "> · started <t:", "· no auto-stop",
		"🔴 idle",
	} {
		if !strings.Contains(r.Data.Content, want) {
			t.Errorf("status lacks %q:\n%s", want, r.Data.Content)
		}
	}
	if r.Data.AllowedMentions == nil || len(r.Data.AllowedMentions.Parse) != 0 {
		t.Errorf("mentions must be disabled: %+v", r.Data.AllowedMentions)
	}
}

func TestStatus_HidesWorkloadsOutsidePolicy(t *testing.T) {
	t.Parallel()

	stopAt := time.Now().Add(time.Hour)
	b, sess, _, _ := newStatusBot(t, []*model.Workload{runningWL("batch", stopAt), runningWL("secret", stopAt)}, policyBatchOnly, mode.Full)

	b.dispatch(statusCommand("u1", roleDevs))
	if c := sess.responses[0].Data.Content; !strings.Contains(c, "batch") || strings.Contains(c, "secret") {
		t.Errorf("devs must see batch only:\n%s", c)
	}

	b.dispatch(statusCommand("u2", roleOther))
	last := sess.responses[len(sess.responses)-1].Data
	if !strings.Contains(last.Content, "No workloads you may operate") || strings.Contains(last.Content, "batch") || len(last.Components) != 0 {
		t.Errorf("a user without permission sees an empty status without buttons: %+v", last)
	}
}

func TestStatus_ButtonsFollowModeAndStopTime(t *testing.T) {
	t.Parallel()

	stopAt := time.Now().Add(time.Hour)
	ws := func() []*model.Workload {
		return []*model.Workload{runningWL("timed", stopAt), runningWL("forever", time.Time{}), wl("idle", 0)}
	}

	b, sess, _, _ := newStatusBot(t, ws(), "", mode.Full)
	b.dispatch(statusCommand("u1"))
	got := statusButtons(sess.responses[0].Data)
	if len(got) != 3 || got["Extend 1h: timed"].Label == "" || got["Stop: timed"].Label == "" || got["Stop: forever"].Label == "" {
		t.Errorf("full mode buttons = %v, want Extend for timed and Stop for timed and forever", got)
	}

	b, sess, _, _ = newStatusBot(t, ws(), "", mode.ScheduledStop)
	b.dispatch(statusCommand("u1"))
	got = statusButtons(sess.responses[0].Data)
	if len(got) != 1 || got["Extend 1h: timed"].Label == "" {
		t.Errorf("scheduled-stop buttons = %v, want only Extend", got)
	}
}

func TestStatus_ExtendButtonMovesStopTimeAndRefreshesTheList(t *testing.T) {
	t.Parallel()

	stopAt := time.Now().Add(time.Hour).Truncate(time.Second)
	b, sess, store, rec := newStatusBot(t, []*model.Workload{runningWL("web", stopAt)}, "", mode.Full)
	b.dispatch(statusCommand("u1"))

	b.dispatch(component("u1", statusButtons(sess.responses[0].Data)["Extend 1h: web"].CustomID))

	if sess.responses[len(sess.responses)-1].Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Error("the button must be acknowledged before the slow work")
	}
	if got := store.stopAts["web"]; !got.Equal(stopAt.Add(time.Hour)) {
		t.Errorf("stop time = %v, want %v", got, stopAt.Add(time.Hour))
	}
	content, _ := lastEdit(t, sess)
	if !strings.Contains(content, "⏰ web now stops at") || !strings.Contains(content, "stops <t:"+fmt.Sprint(stopAt.Add(time.Hour).Unix())+":R>") {
		t.Errorf("refreshed list = %s", content)
	}
	if len(rec.recs) != 1 || rec.recs[0].Event != audit.WorkloadExtend || rec.recs[0].Source != audit.SourceDiscord {
		t.Errorf("audit = %+v, want one workload.extend from discord", rec.recs)
	}
}

func TestStatus_StopButtonStopsAndAudits(t *testing.T) {
	t.Parallel()

	b, sess, store, rec := newStatusBot(t, []*model.Workload{runningWL("web", time.Now().Add(time.Hour))}, "", mode.Full)
	b.dispatch(statusCommand("u1"))

	b.dispatch(component("u1", statusButtons(sess.responses[0].Data)["Stop: web"].CustomID))

	if len(store.stops) != 1 || store.stops[0] != "web" {
		t.Fatalf("stops = %v, want web", store.stops)
	}
	content, rows := lastEdit(t, sess)
	if !strings.Contains(content, "🔴 web → stop") || !strings.Contains(content+"\n", "\n🔴 web\n") {
		t.Errorf("refreshed list = %s", content)
	}
	if len(rows) != 0 {
		t.Errorf("a stopped workload has no buttons left: %d rows", len(rows))
	}
	if len(rec.recs) != 1 || rec.recs[0].Event != audit.WorkloadStop || rec.recs[0].Outcome != audit.Success || rec.recs[0].Source != audit.SourceDiscord {
		t.Errorf("audit = %+v, want one successful workload.stop from discord", rec.recs)
	}
}

func TestStatus_ForgedStopIsRefusedByPolicyAndMode(t *testing.T) {
	t.Parallel()

	// Policy: a stale button of a user who lost the permission is refused at click time.
	stopAt := time.Now().Add(time.Hour)
	b, sess, store, rec := newStatusBot(t, []*model.Workload{runningWL("batch", stopAt)}, policyBatchOnly, mode.Full)
	b.dispatch(statusCommand("u1", roleDevs))
	stop := statusButtons(sess.responses[0].Data)["Stop: batch"].CustomID
	b.dispatch(componentWithRoles("u1", stop, []string{roleOther}))
	if len(store.stops) != 0 {
		t.Errorf("a user without permission stopped %v", store.stops)
	}
	if content, _ := lastEdit(t, sess); !strings.Contains(content, "⛔") {
		t.Errorf("refusal missing from %s", content)
	}
	if len(rec.recs) == 0 || rec.recs[0].Outcome != audit.Denied {
		t.Errorf("audit = %+v, want a denied event", rec.recs)
	}

	// Mode: a Stop token minted while stops were allowed does not work in scheduled-stop mode.
	b, sess, store, _ = newStatusBot(t, []*model.Workload{runningWL("web", stopAt)}, "", mode.ScheduledStop)
	token, err := b.poster.buttons.put(frontend.EncodeStatusRef(frontend.StatusRef{Env: "stg", Namespace: "labs", Names: []string{"web"}}))
	if err != nil {
		t.Fatal(err)
	}
	b.dispatch(component("u1", customID{kindStatusStop, token, 0}.String()))
	if len(store.stops) != 0 {
		t.Errorf("stop ran in scheduled-stop mode: %v", store.stops)
	}
	if content, _ := lastEdit(t, sess); !strings.Contains(content, "stopping from chat is disabled") {
		t.Errorf("mode refusal missing from %s", content)
	}
}

func TestStatus_UnknownOrExpiredButton(t *testing.T) {
	t.Parallel()

	b, sess, _, _ := newStatusBot(t, nil, "", mode.Full)
	b.dispatch(component("u1", customID{kindStatusStop, "nosuchtoken", 0}.String()))
	if got := sess.responses[0].Data; got.Content != msgExpired || got.Flags != discordgo.MessageFlagsEphemeral {
		t.Errorf("response = %+v, want the ephemeral expired notice", got)
	}

	token, _ := b.poster.buttons.put("not json")
	b.dispatch(component("u1", customID{kindStatusExtend, token, 0}.String()))
	if got := sess.responses[1].Data; got.Content != msgUnrecognized {
		t.Errorf("response = %+v, want the unrecognized notice", got)
	}
}

func TestStatus_ButtonCapKeepsToDiscordLimits(t *testing.T) {
	t.Parallel()

	stopAt := time.Now().Add(time.Hour)
	var ws []*model.Workload
	for i := range 20 {
		ws = append(ws, runningWL(fmt.Sprintf("svc-%02d", i), stopAt))
	}
	b, sess, _, _ := newStatusBot(t, ws, "", mode.Full)
	b.dispatch(statusCommand("u1"))

	d := sess.responses[0].Data
	buttons := statusButtons(d)
	if len(d.Components) > 5 || len(buttons) > maxMessageButtons || len(buttons) < 24 {
		t.Errorf("rows=%d buttons=%d, want at most 5 rows and 25 buttons", len(d.Components), len(buttons))
	}
	for _, c := range d.Components {
		if n := len(c.(discordgo.ActionsRow).Components); n > 5 {
			t.Errorf("row with %d buttons", n)
		}
	}
	if len(d.Content) > contentMaxLen || !strings.Contains(d.Content, "entries have none") {
		t.Errorf("content len=%d, want a note about entries without buttons:\n%s", len(d.Content), d.Content)
	}
}

func TestCommand_StartSubcommandOpensTheMenu(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, []*model.Workload{wl("batch", 0)})
	i := command("u1", "knock-stg")
	i.Data = discordgo.ApplicationCommandInteractionData{
		Name:    "knock-stg",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: subcommandStart, Type: discordgo.ApplicationCommandOptionSubCommand}},
	}
	b.dispatch(i)
	if sess.responses[0].Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Fatalf("first response = %+v, want the deferral of the start flow", sess.responses[0])
	}
	_, r := lastEdit(t, sess)
	if _, ok := r[0].Components[0].(discordgo.SelectMenu); !ok {
		t.Errorf("start must show the namespace menu: %+v", r)
	}
}

func TestRegisterCommands_StartStatusAndScheduleSubcommands(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, nil)
	if err := b.registerCommands(); err != nil {
		t.Fatal(err)
	}
	opts := sess.overwrote[0].Options
	if len(opts) != 3 || opts[0].Name != "start" || opts[1].Name != "status" || opts[2].Name != "schedule" {
		t.Fatalf("options = %+v, want the start, status and schedule subcommands", opts)
	}
	for _, o := range opts {
		if o.Type != discordgo.ApplicationCommandOptionSubCommand || o.Description == "" || len(o.Description) > 100 {
			t.Errorf("option %+v", o)
		}
	}
}
