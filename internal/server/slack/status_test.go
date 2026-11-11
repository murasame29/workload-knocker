package slackbot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
)

func TestParseSlashArg(t *testing.T) {
	t.Parallel()

	cases := map[string]slashArg{
		"":                argModal,
		"   ":             argModal,
		"status":          argStatus,
		"  Status  ":      argStatus,
		"STATUS":          argStatus,
		"status now":      argUnknown,
		"stat":            argUnknown,
		"start":           argUnknown,
		"statuses":        argUnknown,
		"status\tplease":  argUnknown,
		"<@U123|someone>": argUnknown,
	}
	for in, want := range cases {
		if got := parseSlashArg(in); got != want {
			t.Errorf("parseSlashArg(%q) = %v, want %v", in, got, want)
		}
	}
}

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
			w.Replicas, w.StopAt = 0, time.Time{}
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

type auditSink struct {
	mu   sync.Mutex
	recs []audit.Record
}

func (a *auditSink) Record(_ context.Context, r audit.Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, r)
}

var statusCluster = config.Cluster{
	Env: "stg", Name: "example-staging",
	Provider:  config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
	Frontends: config.ClusterFrontends{Slack: &config.SlackFrontend{SlashCommand: "/knock-stg"}},
}

type posted struct {
	url string
	msg *slack.WebhookMessage
}

// newStatusBot builds a Bot without a Slack connection; webhook posts are recorded.
func newStatusBot(t *testing.T, ws []*model.Workload, policy string, m mode.Mode) (*Bot, *statusStore, *auditSink, *[]posted) {
	t.Helper()
	var p *authz.Policy
	if policy != "" {
		var err error
		if p, err = authz.Parse([]byte(policy)); err != nil {
			t.Fatal(err)
		}
	}
	reg := helperConfig(t, statusCluster)
	holder := config.NewHolder(reg)
	store := &statusStore{workloads: ws}
	rec := &auditSink{}
	az := authz.NewHolder(p)
	svc := frontend.NewService(store, metrics.New(), time.Second,
		frontend.WithAuthorizer(az), frontend.WithMode(m), frontend.WithRecorder(rec), frontend.WithClusters(holder))
	var out []posted
	b := &Bot{
		svc: svc, registry: holder, authz: az,
		webhook: func(_ context.Context, url string, msg *slack.WebhookMessage) error {
			out = append(out, posted{url, msg})
			return nil
		},
	}
	return b, store, rec, &out
}

func runningWL(name string, stopAt time.Time) *model.Workload {
	return &model.Workload{
		Env: "stg", Name: name, Namespace: "labs", Kind: "Deployment", Replicas: 1,
		StartedBy: "alice", StartedAt: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC), StopAt: stopAt,
	}
}

func stoppedWL(name string) *model.Workload {
	return &model.Workload{Env: "stg", Name: name, Namespace: "labs", Kind: "Deployment"}
}

var slackUser = authz.Subject{Platform: authz.PlatformSlack, User: "UALICE111"}

func blockTexts(blocks []slack.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if s, ok := blk.(*slack.SectionBlock); ok && s.Text != nil {
			b.WriteString(s.Text.Text + "\n")
		}
	}
	return b.String()
}

func actionButtons(blocks []slack.Block) map[string]*slack.ButtonBlockElement {
	out := map[string]*slack.ButtonBlockElement{}
	for _, blk := range blocks {
		if a, ok := blk.(*slack.ActionBlock); ok {
			for _, e := range a.Elements.ElementSet {
				btn := e.(*slack.ButtonBlockElement)
				out[btn.Text.Text] = btn
			}
		}
	}
	return out
}

func TestStatusMessage_ListsEntriesWithTimes(t *testing.T) {
	t.Parallel()

	stopAt := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	b, store, _, _ := newStatusBot(t, []*model.Workload{runningWL("shop-api", stopAt), stoppedWL("shop-app"), runningWL("batch", time.Time{}), stoppedWL("idle")}, "", mode.Full)

	text, blocks := statusMessage(statusCluster, b.svc.Status(slackUser, "stg"), nil)

	body := blockTexts(blocks)
	for _, want := range []string{
		"*STG / example-staging* status", "*labs*",
		"🟡 shop (api, app) 1/2 running · by alice · started <!date^" + fmt.Sprint(time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC).Unix()) + "^",
		"· stops <!date^" + fmt.Sprint(stopAt.Unix()) + "^",
		"🟢 batch (1 pod) · by alice", "· no auto-stop",
		"🔴 idle",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("status lacks %q:\n%s", want, body)
		}
	}
	if text == "" || store.reconciles != 0 {
		t.Errorf("fallback text %q, reconciles %d: the list must come from memory without a cluster call", text, store.reconciles)
	}
}

func TestStatusMessage_FiltersByPolicyAndEscapes(t *testing.T) {
	t.Parallel()

	const policy = `
version: 1
rules:
  - env: stg
    namespaces: [labs]
    workloads: [batch]
    subjects: {slack: {users: [UALICE111]}}
`
	hostile := runningWL("batch", time.Now().Add(time.Hour))
	hostile.StartedBy = "<!channel> & co"
	b, _, _, _ := newStatusBot(t, []*model.Workload{hostile, runningWL("secret", time.Time{})}, policy, mode.Full)

	_, blocks := statusMessage(statusCluster, b.svc.Status(slackUser, "stg"), nil)
	body := blockTexts(blocks)
	if !strings.Contains(body, "batch") || strings.Contains(body, "secret") {
		t.Errorf("only batch may be listed:\n%s", body)
	}
	if strings.Contains(body, "<!channel>") || !strings.Contains(body, "&lt;!channel&gt; &amp; co") {
		t.Errorf("the starter must be escaped:\n%s", body)
	}

	other := authz.Subject{Platform: authz.PlatformSlack, User: "UOTHER1111"}
	_, blocks = statusMessage(statusCluster, b.svc.Status(other, "stg"), nil)
	if body := blockTexts(blocks); !strings.Contains(body, "No workloads you may operate") || strings.Contains(body, "batch") || len(actionButtons(blocks)) != 0 {
		t.Errorf("a user without permission sees nothing:\n%s", body)
	}
}

func TestStatusMessage_ButtonsFollowModeAndStopTime(t *testing.T) {
	t.Parallel()

	stopAt := time.Now().Add(time.Hour)
	ws := func() []*model.Workload {
		return []*model.Workload{runningWL("timed", stopAt), runningWL("forever", time.Time{}), stoppedWL("idle")}
	}

	b, _, _, _ := newStatusBot(t, ws(), "", mode.Full)
	_, blocks := statusMessage(statusCluster, b.svc.Status(slackUser, "stg"), nil)
	got := actionButtons(blocks)
	if len(got) != 3 || got["Extend 1h: timed"] == nil || got["Stop: timed"] == nil || got["Stop: forever"] == nil {
		t.Errorf("full mode buttons = %v", got)
	}
	if got["Stop: timed"].Style != slack.StyleDanger || got["Stop: timed"].ActionID != actionStatusStop || got["Extend 1h: timed"].ActionID != actionStatusExtend {
		t.Errorf("button wiring = %+v / %+v", got["Stop: timed"], got["Extend 1h: timed"])
	}

	b, _, _, _ = newStatusBot(t, ws(), "", mode.ScheduledStop)
	_, blocks = statusMessage(statusCluster, b.svc.Status(slackUser, "stg"), nil)
	got = actionButtons(blocks)
	if len(got) != 1 || got["Extend 1h: timed"] == nil {
		t.Errorf("scheduled-stop buttons = %v, want only Extend", got)
	}
}

func TestStatusMessage_StaysWithinSlackLimits(t *testing.T) {
	t.Parallel()

	stopAt := time.Now().Add(time.Hour)
	var ws []*model.Workload
	for i := range 80 {
		ws = append(ws, runningWL(fmt.Sprintf("service-with-a-long-name-%03d", i), stopAt))
	}
	b, _, _, _ := newStatusBot(t, ws, "", mode.Full)

	_, blocks := statusMessage(statusCluster, b.svc.Status(slackUser, "stg"), nil)

	if len(blocks) > slackMessageMaxBlocks {
		t.Errorf("%d blocks, Slack allows %d", len(blocks), slackMessageMaxBlocks)
	}
	for _, blk := range blocks {
		if s, ok := blk.(*slack.SectionBlock); ok && len(s.Text.Text) > slackSectionTextMaxLen {
			t.Errorf("section of %d chars exceeds %d", len(s.Text.Text), slackSectionTextMaxLen)
		}
	}
	if !strings.Contains(blockTexts(blocks), "entries have no buttons") {
		t.Error("a note must say that some entries have no buttons")
	}
}

func actionCallback(user, actionID, value string) slack.InteractionCallback {
	cb := slack.InteractionCallback{Type: slack.InteractionTypeBlockActions, ResponseURL: "https://hooks.slack.test/actions/T1/1/abc"}
	cb.User.ID, cb.User.Name = user, "alice"
	cb.ActionCallback.BlockActions = []*slack.BlockAction{{ActionID: actionID, Value: value}}
	return cb
}

func TestStatusAction_ExtendReplacesTheListAndAudits(t *testing.T) {
	t.Parallel()

	stopAt := time.Now().Add(time.Hour).Truncate(time.Second)
	b, store, rec, out := newStatusBot(t, []*model.Workload{runningWL("web", stopAt)}, "", mode.Full)
	_, blocks := statusMessage(statusCluster, b.svc.Status(slackUser, "stg"), nil)
	btn := actionButtons(blocks)["Extend 1h: web"]

	cb := actionCallback(slackUser.User, btn.ActionID, btn.Value)
	b.handleInteraction(cb)

	if got := store.stopAts["web"]; !got.Equal(stopAt.Add(time.Hour)) {
		t.Errorf("stop time = %v, want %v", got, stopAt.Add(time.Hour))
	}
	if len(*out) != 1 || (*out)[0].url != cb.ResponseURL || !(*out)[0].msg.ReplaceOriginal || (*out)[0].msg.ResponseType != slack.ResponseTypeEphemeral {
		t.Fatalf("posted = %+v, want one ephemeral replace_original to the response_url", *out)
	}
	if body := blockTexts((*out)[0].msg.Blocks.BlockSet); !strings.Contains(body, "⏰ web now stops at") {
		t.Errorf("refreshed list lacks the outcome:\n%s", body)
	}
	if len(rec.recs) != 1 || rec.recs[0].Event != audit.WorkloadExtend || rec.recs[0].Source != audit.SourceSlack {
		t.Errorf("audit = %+v, want one workload.extend from slack", rec.recs)
	}
}

func TestStatusAction_StopStopsAndAudits(t *testing.T) {
	t.Parallel()

	b, store, rec, out := newStatusBot(t, []*model.Workload{runningWL("web", time.Now().Add(time.Hour))}, "", mode.Full)
	_, blocks := statusMessage(statusCluster, b.svc.Status(slackUser, "stg"), nil)
	btn := actionButtons(blocks)["Stop: web"]

	cb := actionCallback(slackUser.User, btn.ActionID, btn.Value)
	b.handleInteraction(cb)

	if len(store.stops) != 1 || store.stops[0] != "web" {
		t.Fatalf("stops = %v", store.stops)
	}
	body := blockTexts((*out)[0].msg.Blocks.BlockSet)
	if !strings.Contains(body, "🔴 web → stop") || len(actionButtons((*out)[0].msg.Blocks.BlockSet)) != 0 {
		t.Errorf("refreshed list = %s", body)
	}
	if len(rec.recs) != 1 || rec.recs[0].Event != audit.WorkloadStop || rec.recs[0].Outcome != audit.Success || rec.recs[0].Source != audit.SourceSlack {
		t.Errorf("audit = %+v, want one successful workload.stop from slack", rec.recs)
	}
}

func TestStatusAction_RefusedByPolicyAndMode(t *testing.T) {
	t.Parallel()

	const policy = `
version: 1
rules:
  - env: stg
    namespaces: [labs]
    subjects: {slack: {users: [UALICE111]}}
`
	payload := frontend.EncodeStatusRef(frontend.StatusRef{Env: "stg", Namespace: "labs", Names: []string{"web"}})

	b, store, _, out := newStatusBot(t, []*model.Workload{runningWL("web", time.Now().Add(time.Hour))}, policy, mode.Full)
	cb := actionCallback("UINTRUDER1", actionStatusStop, payload)
	b.handleInteraction(cb)
	if len(store.stops) != 0 || !strings.Contains(blockTexts((*out)[0].msg.Blocks.BlockSet), "⛔") {
		t.Errorf("an unauthorized user stopped %v; posted %+v", store.stops, *out)
	}

	b, store, _, out = newStatusBot(t, []*model.Workload{runningWL("web", time.Now().Add(time.Hour))}, "", mode.ScheduledStop)
	cb = actionCallback(slackUser.User, actionStatusStop, payload)
	b.handleInteraction(cb)
	if len(store.stops) != 0 || !strings.Contains(blockTexts((*out)[0].msg.Blocks.BlockSet), "stopping from chat is disabled") {
		t.Errorf("stop ran in scheduled-stop mode: %v; posted %+v", store.stops, *out)
	}
}

func TestStatusAction_InvalidPayloadPostsNothing(t *testing.T) {
	t.Parallel()

	b, store, _, out := newStatusBot(t, []*model.Workload{runningWL("web", time.Now().Add(time.Hour))}, "", mode.Full)
	cb := actionCallback(slackUser.User, actionStatusStop, "not json")
	b.handleInteraction(cb)
	if len(*out) != 0 || len(store.stops) != 0 {
		t.Errorf("posted=%d stops=%v, want nothing for an invalid payload", len(*out), store.stops)
	}
}
