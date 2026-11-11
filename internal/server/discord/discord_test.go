package discordbot

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
)

func TestCustomID_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []customID{
		{kindNamespaceSelect, "a1b2c3d4e5f6", 0},
		{kindWorkloadPage, "a1b2c3d4e5f6", 7},
		{kindConfirm, "000000000000", 0},
	}
	for _, want := range cases {
		s := want.String()
		if len(s) >= 100 {
			t.Errorf("custom_id %q is %d chars, want < 100", s, len(s))
		}
		got, err := parseCustomID(s)
		if err != nil || got != want {
			t.Errorf("parseCustomID(%q) = %+v, %v; want %+v", s, got, err, want)
		}
	}
}

func TestParseCustomID_Invalid(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "wk2:ns:abc:0", "wk1:ns:abc", "wk1:ns:abc:x", "wk1:ns:abc:-1", "wk1::abc:0", "wk1:ns::0", "wk1:ns:abc:0:1"} {
		if _, err := parseCustomID(s); err == nil {
			t.Errorf("parseCustomID(%q): want error", s)
		}
	}
}

func TestPageBounds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		total, page                   int
		wantPage, wantS, wantE, wantN int
	}{
		{0, 0, 0, 0, 0, 0},
		{10, 0, 0, 0, 10, 1},
		{25, 0, 0, 0, 25, 1},
		{26, 1, 1, 25, 26, 2},
		{26, 9, 1, 25, 26, 2},
		{60, -3, 0, 0, 25, 3},
		{60, 2, 2, 50, 60, 3},
	}
	for _, c := range cases {
		p, s, e, n := pageBounds(c.total, selectMaxOptions, c.page)
		if p != c.wantPage || s != c.wantS || e != c.wantE || n != c.wantN {
			t.Errorf("pageBounds(%d, 25, %d) = %d,%d,%d,%d; want %d,%d,%d,%d", c.total, c.page, p, s, e, n, c.wantPage, c.wantS, c.wantE, c.wantN)
		}
	}
}

func wl(name string, replicas int) *model.Workload {
	return &model.Workload{Name: name, Namespace: "labs", Replicas: replicas}
}

func testCluster() config.Cluster {
	return config.Cluster{
		Env:       "stg",
		Name:      "example-staging",
		Provider:  config.ClusterProvider{Type: config.ProviderTypeEKS, EKS: &config.ProviderEKS{Region: "ap-northeast-1"}},
		Frontends: config.ClusterFrontends{Discord: &config.DiscordFrontend{Command: "knock-stg"}},
	}
}

func rows(v view) []discordgo.ActionsRow {
	var out []discordgo.ActionsRow
	for _, c := range v.Components {
		out = append(out, c.(discordgo.ActionsRow))
	}
	return out
}

func TestNamespaceView_Pagination(t *testing.T) {
	t.Parallel()

	var ns []string
	for i := range 30 {
		ns = append(ns, fmt.Sprintf("ns-%02d", i))
	}
	v := namespaceView("tok", testCluster(), ns, 1, 0)
	r := rows(v)
	menu := r[0].Components[0].(discordgo.SelectMenu)
	if len(menu.Options) != 5 || menu.Options[0].Value != "ns-25" || menu.MaxValues != 1 {
		t.Errorf("page 1 options = %d first=%q max=%d", len(menu.Options), menu.Options[0].Value, menu.MaxValues)
	}
	prev := r[1].Components[0].(discordgo.Button)
	next := r[1].Components[1].(discordgo.Button)
	if prev.Disabled || !next.Disabled {
		t.Errorf("prev.Disabled=%v next.Disabled=%v; want false/true on the last page", prev.Disabled, next.Disabled)
	}
	if !strings.Contains(v.Content, "page 2/2") {
		t.Errorf("content %q lacks page info", v.Content)
	}

	one := rows(namespaceView("tok", testCluster(), ns[:3], 0, 0))
	if len(one[1].Components) != 1 {
		t.Errorf("single page: want only Cancel, got %d buttons", len(one[1].Components))
	}
}

func TestWorkloadView_OptionsLimitsAndDefaults(t *testing.T) {
	t.Parallel()

	var ws []*model.Workload
	for i := range 40 {
		replicas := 0
		if i == 1 {
			replicas = 1
		}
		ws = append(ws, wl(fmt.Sprintf("svc-%02d", i), replicas))
	}
	ws = append(ws, wl(strings.Repeat("x", 120), 0))
	entries := frontend.GroupEntries(ws)
	selected := map[string]bool{"svc-01": true}

	r := rows(workloadView("tok", testCluster(), "labs", entries, selected, 0, true))
	menu := r[0].Components[0].(discordgo.SelectMenu)
	if len(menu.Options) != 25 || menu.MaxValues != 25 || *menu.MinValues != 0 {
		t.Fatalf("options=%d max=%d min=%d", len(menu.Options), menu.MaxValues, *menu.MinValues)
	}
	for i, o := range menu.Options {
		if o.Value != fmt.Sprint(i) {
			t.Errorf("option %d value = %q, want the entry index", i, o.Value)
		}
		if o.Default != (i == 1) {
			t.Errorf("option %d default = %v", i, o.Default)
		}
	}

	last := rows(workloadView("tok", testCluster(), "labs", entries, selected, 1, true))[0].Components[0].(discordgo.SelectMenu)
	if len(last.Options) != 16 {
		t.Fatalf("page 1 options = %d, want 16", len(last.Options))
	}
	for _, o := range last.Options {
		if n := len([]rune(o.Label)); n > optionLabelMaxLen {
			t.Errorf("label length %d exceeds %d", n, optionLabelMaxLen)
		}
	}
	if len(r[1].Components) != 5 || r[1].Components[3].(discordgo.Button).Label != "Other namespace" {
		t.Errorf("buttons = %+v, want Previous, Next, Apply, Other namespace, Cancel", r[1].Components)
	}

	empty := workloadView("tok", testCluster(), "labs", nil, nil, 0, true)
	if got := len(rows(empty)[0].Components); got != 2 || !strings.Contains(empty.Content, "No workloads") {
		t.Errorf("empty view: %d components, content %q", got, empty.Content)
	}
}

func TestPreviewView_TextAndTruncation(t *testing.T) {
	t.Parallel()

	changes := []frontend.Change{
		{Action: frontend.ActionStart, Label: "checkout (app, worker)"},
		{Action: frontend.ActionStop, Label: "batch"},
	}
	v := previewView("tok", testCluster(), []section{{namespace: "labs", changes: changes, rejected: nil, starts: nil}})
	for _, want := range []string{"**STG / example-staging**", "labs change preview:", "🟢 checkout (app, worker): stopped → **start**", "🔴 batch: running → **stop**"} {
		if !strings.Contains(v.Content, want) {
			t.Errorf("preview lacks %q:\n%s", want, v.Content)
		}
	}
	btns := rows(v)[0].Components
	if btns[0].(discordgo.Button).Label != "Confirm" || btns[1].(discordgo.Button).Label != "Cancel" {
		t.Errorf("buttons = %+v", btns)
	}

	var many []frontend.Change
	for i := range 200 {
		many = append(many, frontend.Change{Action: frontend.ActionStart, Label: fmt.Sprintf("workload-with-a-long-name-%03d", i)})
	}
	big := previewView("tok", testCluster(), []section{{namespace: "labs", changes: many, rejected: nil, starts: nil}}).Content
	if len(big) > contentMaxLen || !strings.Contains(big, "more") {
		t.Errorf("long preview len=%d, want <= %d with a remainder note", len(big), contentMaxLen)
	}
}

func TestFlowStore_TTLAndTake(t *testing.T) {
	t.Parallel()

	s := newFlowStore(time.Minute)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }

	tok, err := s.create(&flow{userID: "u1"})
	if err != nil || len(tok) != 12 {
		t.Fatalf("create: %q %v", tok, err)
	}
	if !s.with(tok, func(f *flow) { f.nsPage = 3 }) {
		t.Fatal("fresh flow not found")
	}
	now = now.Add(59 * time.Second)
	if !s.with(tok, func(*flow) {}) {
		t.Fatal("with must extend the TTL")
	}
	now = now.Add(2 * time.Minute)
	if s.with(tok, func(*flow) {}) {
		t.Error("expired flow still usable")
	}
	if _, ok := s.take(tok); ok {
		t.Error("expired flow was taken")
	}
}

func TestFlowStore_BoundedSize(t *testing.T) {
	t.Parallel()

	s := newFlowStore(time.Hour)
	for range maxFlows + 50 {
		if _, err := s.create(&flow{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.flows) > maxFlows {
		t.Errorf("flows = %d, want <= %d", len(s.flows), maxFlows)
	}
}

// fakeStore is a minimal in-memory model.WorkloadStore.
type fakeStore struct {
	model.WorkloadStore
	mu        sync.Mutex
	workloads []*model.Workload
	calls     []string
}

func (f *fakeStore) Namespaces(string) []string { return []string{"labs"} }
func (f *fakeStore) WorkloadsByNamespace(string, string) []*model.Workload {
	return f.workloads
}
func (f *fakeStore) Start(_, _, name, user string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start:"+name+":"+user)
	return nil
}
func (f *fakeStore) Stop(_, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop:"+name)
	return nil
}
func (f *fakeStore) Reconcile(context.Context, string) error { return nil }

// fakeSession records responses instead of talking to Discord.
type fakeSession struct {
	responses []*discordgo.InteractionResponse
	edits     []*discordgo.WebhookEdit
	overwrote []*discordgo.ApplicationCommand
	guild     string
	// overwrites counts OverwriteCommands calls; overwriteErr makes them fail.
	overwrites   int
	overwriteErr error
	// sent / edited record channel messages posted and edited through the notify poster.
	sent   []sentMessage
	edited []*discordgo.MessageEdit
}

type sentMessage struct {
	channel string
	msg     *discordgo.MessageSend
}

func (s *fakeSession) SendMessage(channel string, m *discordgo.MessageSend) (string, error) {
	s.sent = append(s.sent, sentMessage{channel, m})
	return fmt.Sprintf("msg-%d", len(s.sent)), nil
}

func (s *fakeSession) EditMessage(e *discordgo.MessageEdit) error {
	s.edited = append(s.edited, e)
	return nil
}

func (s *fakeSession) Open() error           { return nil }
func (s *fakeSession) Close() error          { return nil }
func (s *fakeSession) AddHandler(any) func() { return func() {} }
func (s *fakeSession) AppID() string         { return "app" }
func (s *fakeSession) OverwriteCommands(g string, c []*discordgo.ApplicationCommand) error {
	if s.overwriteErr != nil {
		return s.overwriteErr
	}
	s.overwrites++
	s.guild, s.overwrote = g, c
	return nil
}
func (s *fakeSession) Respond(_ *discordgo.Interaction, r *discordgo.InteractionResponse) error {
	s.responses = append(s.responses, r)
	return nil
}
func (s *fakeSession) Edit(_ *discordgo.Interaction, e *discordgo.WebhookEdit) error {
	s.edits = append(s.edits, e)
	return nil
}

func newTestBot(t *testing.T, ws []*model.Workload) (*Bot, *fakeSession, *fakeStore) {
	t.Helper()
	return newTestBotWith(t, ws, "", mode.Full)
}

// newTestBotWith builds a Bot with an authorization policy (YAML, "" = disabled) and operation mode.
func newTestBotWith(t *testing.T, ws []*model.Workload, policy string, m mode.Mode) (*Bot, *fakeSession, *fakeStore) {
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
	store := &fakeStore{workloads: ws}
	sess := &fakeSession{}
	svc := frontend.NewService(store, metrics.New(), time.Second,
		frontend.WithAuthorizer(authz.NewHolder(p)), frontend.WithMode(m))
	b := newBot(sess, svc, config.NewHolder(reg), "guild-1")
	// Most tests drive the message flow; modal_test.go turns the modal back on.
	b.noModal = true
	return b, sess, store
}

func member(id string) *discordgo.Member { return &discordgo.Member{User: &discordgo.User{ID: id}} }

func command(user, name string) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type:   discordgo.InteractionApplicationCommand,
		Member: member(user),
		Data:   discordgo.ApplicationCommandInteractionData{Name: name},
	}
}

func component(user, customID string, values ...string) *discordgo.Interaction {
	return &discordgo.Interaction{
		Type:   discordgo.InteractionMessageComponent,
		Member: member(user),
		Data:   discordgo.MessageComponentInteractionData{CustomID: customID, Values: values},
	}
}

func lastEdit(t *testing.T, s *fakeSession) (string, []discordgo.ActionsRow) {
	t.Helper()
	e := s.edits[len(s.edits)-1]
	var r []discordgo.ActionsRow
	for _, c := range *e.Components {
		r = append(r, c.(discordgo.ActionsRow))
	}
	return *e.Content, r
}

func lastUpdate(t *testing.T, s *fakeSession) *discordgo.InteractionResponseData {
	t.Helper()
	r := s.responses[len(s.responses)-1]
	if r.Type != discordgo.InteractionResponseUpdateMessage {
		t.Fatalf("response type = %v, want UpdateMessage", r.Type)
	}
	return r.Data
}

func TestRegisterCommands(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, nil)
	if err := b.registerCommands(); err != nil {
		t.Fatal(err)
	}
	if sess.guild != "guild-1" || len(sess.overwrote) != 1 || sess.overwrote[0].Name != "knock-stg" ||
		sess.overwrote[0].Description != "Start or stop workloads in stg" {
		t.Errorf("guild=%q cmds=%+v", sess.guild, sess.overwrote)
	}
}

func TestFlow_CommandToApply(t *testing.T) {
	t.Parallel()

	b, sess, store := newTestBot(t, []*model.Workload{wl("shop-api", 1), wl("shop-app", 0), wl("batch", 0), wl("legacy", 1)})

	b.dispatch(command("u1", "knock-stg"))
	if sess.responses[0].Type != discordgo.InteractionResponseDeferredChannelMessageWithSource ||
		sess.responses[0].Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("first response must be an ephemeral deferral: %+v", sess.responses[0])
	}
	_, r := lastEdit(t, sess)
	nsMenu := r[0].Components[0].(discordgo.SelectMenu)
	nsID, err := parseCustomID(nsMenu.CustomID)
	if err != nil || nsID.Kind != kindNamespaceSelect {
		t.Fatalf("namespace menu id = %q (%v)", nsMenu.CustomID, err)
	}

	// Another user cannot drive this flow.
	b.dispatch(component("intruder", nsMenu.CustomID, "labs"))
	if got := sess.responses[len(sess.responses)-1].Data; got.Content != msgForeign || got.Flags != discordgo.MessageFlagsEphemeral {
		t.Errorf("foreign user response = %+v", got)
	}

	// Choosing the namespace preselects running entries (shop partial, legacy running).
	b.dispatch(component("u1", nsMenu.CustomID, "labs"))
	wlRows := lastUpdate(t, sess).Components
	wlMenu := wlRows[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	var pre []string
	for _, o := range wlMenu.Options {
		if o.Default {
			pre = append(pre, o.Value)
		}
	}
	if len(wlMenu.Options) != 3 || !slices.Equal(pre, []string{"1", "2"}) {
		t.Fatalf("options=%d preselected=%v; entries sort as batch, legacy, shop", len(wlMenu.Options), pre)
	}

	// Deselect legacy, select batch: stop legacy, start batch and shop-app.
	b.dispatch(component("u1", wlMenu.CustomID, "0", "2"))
	rr := lastUpdate(t, sess).Components[1].(discordgo.ActionsRow).Components
	var applyID, confirmID string
	for _, c := range rr {
		if btn := c.(discordgo.Button); btn.Label == "Apply" {
			applyID = btn.CustomID
		}
	}
	b.dispatch(component("u1", applyID))
	prev := lastUpdate(t, sess)
	for _, want := range []string{"🟢 batch: stopped → **start**", "🟢 shop (app): stopped → **start**", "🔴 legacy: running → **stop**"} {
		if !strings.Contains(prev.Content, want) {
			t.Errorf("preview lacks %q:\n%s", want, prev.Content)
		}
	}
	for _, c := range prev.Components[0].(discordgo.ActionsRow).Components {
		if btn := c.(discordgo.Button); btn.Label == "Confirm" {
			confirmID = btn.CustomID
		}
	}

	b.dispatch(component("u1", confirmID))
	if sess.responses[len(sess.responses)-1].Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Error("confirm must defer before applying")
	}
	got := *sess.edits[len(sess.edits)-1].Content
	if !strings.Contains(got, "<@u1> changed workloads") || strings.Contains(got, "22:00 JST") {
		t.Errorf("result = %q", got)
	}
	slices.Sort(store.calls)
	want := []string{"start:batch:u1", "start:shop-app:u1", "stop:legacy"}
	if !slices.Equal(store.calls, want) {
		t.Errorf("store calls = %v, want %v", store.calls, want)
	}

	// A second Confirm click must not apply again.
	b.dispatch(component("u1", confirmID))
	if len(store.calls) != 3 {
		t.Errorf("confirm applied twice: %v", store.calls)
	}
}

func TestFlow_NoChangesAndExpired(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, []*model.Workload{wl("batch", 1)})
	b.dispatch(command("u1", "knock-stg"))
	_, r := lastEdit(t, sess)
	nsMenu := r[0].Components[0].(discordgo.SelectMenu)
	b.dispatch(component("u1", nsMenu.CustomID, "labs"))
	rr := lastUpdate(t, sess).Components[1].(discordgo.ActionsRow).Components
	apply := rr[0].(discordgo.Button).CustomID
	b.dispatch(component("u1", apply))
	if c := lastUpdate(t, sess).Content; !strings.Contains(c, "no changes") {
		t.Errorf("content = %q", c)
	}
	b.dispatch(component("u1", nsMenu.CustomID, "labs"))
	if c := lastUpdate(t, sess).Content; c != msgExpired {
		t.Errorf("finished flow content = %q, want expired", c)
	}
}

func TestDispatch_UnknownCommandAndBadID(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, nil)
	b.dispatch(command("u1", "nope"))
	b.dispatch(component("u1", "garbage"))
	if len(sess.responses) != 2 || len(sess.edits) != 0 {
		t.Fatalf("responses=%d edits=%d", len(sess.responses), len(sess.edits))
	}
	for _, r := range sess.responses {
		if r.Data.Flags != discordgo.MessageFlagsEphemeral {
			t.Errorf("response not ephemeral: %+v", r)
		}
	}
}

func TestApplyPageSelection_KeepsOtherPages(t *testing.T) {
	t.Parallel()

	var ws []*model.Workload
	for i := range 30 {
		ws = append(ws, wl(fmt.Sprintf("svc-%02d", i), 0))
	}
	f := &nsPart{entries: frontend.GroupEntries(ws), page: 1, selected: map[string]bool{"svc-00": true, "svc-26": true}}
	applyPageSelection(f, []string{"27", "3", "bad"})
	if !f.selected["svc-00"] || f.selected["svc-26"] || !f.selected["svc-27"] || f.selected["svc-03"] {
		t.Errorf("selected = %v", f.selected)
	}
}

func TestInteractionUser(t *testing.T) {
	t.Parallel()

	if id, _ := interactionUser(&discordgo.Interaction{Member: member("m")}); id != "m" {
		t.Errorf("member id = %q", id)
	}
	if id, _ := interactionUser(&discordgo.Interaction{User: &discordgo.User{ID: "d"}}); id != "d" {
		t.Errorf("dm id = %q", id)
	}
	if id, _ := interactionUser(&discordgo.Interaction{}); id != "" {
		t.Errorf("id = %q", id)
	}
}
