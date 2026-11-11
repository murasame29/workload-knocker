package discordbot

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
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

// nsStore holds workloads of several namespaces and records store calls as "start:ns/name".
type nsStore struct {
	model.WorkloadStore
	mu         sync.Mutex
	byNS       map[string][]*model.Workload
	calls      []string
	reconciles int
}

func (s *nsStore) Namespaces(string) []string {
	out := make([]string, 0, len(s.byNS))
	for ns := range s.byNS {
		out = append(out, ns)
	}
	return out
}
func (s *nsStore) WorkloadsByNamespace(_, ns string) []*model.Workload { return s.byNS[ns] }
func (s *nsStore) Start(_, ns, name, _ string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "start:"+ns+"/"+name)
	return nil
}
func (s *nsStore) Stop(_, ns, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "stop:"+ns+"/"+name)
	return nil
}
func (s *nsStore) SetStopAt(string, string, string, time.Time) bool { return true }
func (s *nsStore) Reconcile(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciles++
	return nil
}

func nsWorkloads(ns string, replicas ...int) []*model.Workload {
	out := make([]*model.Workload, 0, len(replicas))
	for i, r := range replicas {
		out = append(out, &model.Workload{Name: fmt.Sprintf("%s-w%02d", ns, i), Namespace: ns, Kind: "Deployment", Replicas: r})
	}
	return out
}

// modalBot builds a Bot with the modal enabled over byNS; policy is YAML ("" = disabled).
func modalBot(t *testing.T, byNS map[string][]*model.Workload, policy string) (*Bot, *fakeSession, *nsStore) {
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
	store := &nsStore{byNS: byNS}
	sess := &fakeSession{}
	svc := frontend.NewService(store, metrics.New(), time.Second, frontend.WithAuthorizer(authz.NewHolder(p)), frontend.WithMode(mode.Full))
	return newBot(sess, svc, config.NewHolder(reg), "guild-1"), sess, store
}

// modalJSON is the wire shape of the selection modal this package sends.
type modalJSON struct {
	CustomID   string `json:"custom_id"`
	Title      string `json:"title"`
	Components []struct {
		Type      int    `json:"type"`
		Label     string `json:"label"`
		Component struct {
			Type      int    `json:"type"`
			CustomID  string `json:"custom_id"`
			MinValues int    `json:"min_values"`
			MaxValues int    `json:"max_values"`
			Required  bool   `json:"required"`
			Options   []struct {
				Value   string `json:"value"`
				Label   string `json:"label"`
				Default bool   `json:"default"`
			} `json:"options"`
		} `json:"component"`
	} `json:"components"`
}

func openModal(t *testing.T, b *Bot, sess *fakeSession) modalJSON {
	t.Helper()
	return openModalAs(t, b, sess, "u1")
}

func openModalAs(t *testing.T, b *Bot, sess *fakeSession, user string) modalJSON {
	t.Helper()
	b.dispatch(command(user, "knock-stg"))
	r := sess.responses[len(sess.responses)-1]
	if r.Type != discordgo.InteractionResponseModal {
		t.Fatalf("response type = %v, want a modal", r.Type)
	}
	raw, err := json.Marshal(r.Data)
	if err != nil {
		t.Fatal(err)
	}
	var m modalJSON
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// submit builds the raw INTERACTION_CREATE payload of a selection modal submission.
func submit(t *testing.T, user, customID string, checked map[string][]string) *discordgo.Event {
	t.Helper()
	keys := make([]string, 0, len(checked))
	for k := range checked {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	comps := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		comps = append(comps, map[string]any{"type": 18, "id": 1, "component": map[string]any{"type": 22, "id": 2, "custom_id": k, "values": checked[k]}})
	}
	raw, err := json.Marshal(map[string]any{
		"id": "i1", "application_id": "app", "type": 5, "token": "tok",
		"member": map[string]any{"user": map[string]any{"id": user}},
		"data":   map[string]any{"custom_id": customID, "components": comps},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &discordgo.Event{Type: "INTERACTION_CREATE", RawData: raw}
}

func TestSelectionModal_OneGroupPerNamespaceWithoutReconcile(t *testing.T) {
	t.Parallel()

	b, sess, store := modalBot(t, map[string][]*model.Workload{
		"alpha": nsWorkloads("alpha", 1, 0),
		"beta":  nsWorkloads("beta", 0),
	}, "")
	m := openModal(t, b, sess)

	if store.reconciles != 0 {
		t.Errorf("reconciles = %d, want none on the command path", store.reconciles)
	}
	if len(m.Components) != 2 || m.Components[0].Label != "alpha" || m.Components[1].Label != "beta" {
		t.Fatalf("labels = %+v, want alpha, beta", m.Components)
	}
	g := m.Components[0].Component
	if m.Components[0].Type != 18 || g.Type != 22 || g.CustomID != "0" || g.MinValues != 0 || g.Required || g.MaxValues != 2 {
		t.Errorf("label/group = %d/%+v, want an optional Checkbox Group in a Label", m.Components[0].Type, g)
	}
	if len(g.Options) != 2 || !g.Options[0].Default || g.Options[1].Default || g.Options[0].Value != "0" {
		t.Errorf("options = %+v, want the running entry checked", g.Options)
	}
	if !strings.HasPrefix(m.CustomID, customIDVersion+":"+kindSelectModal+":") || m.Title != "Workloads in STG" {
		t.Errorf("custom_id %q title %q", m.CustomID, m.Title)
	}
}

func TestSelectionModal_FallsBackOverTheLimits(t *testing.T) {
	t.Parallel()

	tooManyNS := map[string][]*model.Workload{}
	for i := range modalMaxLabels + 1 {
		ns := fmt.Sprintf("ns-%d", i)
		tooManyNS[ns] = nsWorkloads(ns, 0)
	}
	tooManyEntries := map[string][]*model.Workload{"big": nsWorkloads("big", make([]int, checkboxMaxOptions+1)...)}

	for name, byNS := range map[string]map[string][]*model.Workload{"namespaces": tooManyNS, "entries": tooManyEntries} {
		b, sess, store := modalBot(t, byNS, "")
		b.dispatch(command("u1", "knock-stg"))
		if sess.responses[0].Type != discordgo.InteractionResponseDeferredChannelMessageWithSource || store.reconciles != 1 {
			t.Errorf("%s: response %v, reconciles %d, want the deferred message flow", name, sess.responses[0].Type, store.reconciles)
			continue
		}
		if _, r := lastEdit(t, sess); len(r) == 0 || r[0].Components[0].(discordgo.SelectMenu).Placeholder != "Select a namespace" {
			t.Errorf("%s: fallback did not show the namespace menu", name)
		}
	}
}

func TestSelectionModal_SubmitPreviewsAndAppliesEveryNamespace(t *testing.T) {
	t.Parallel()

	b, sess, store := modalBot(t, map[string][]*model.Workload{
		"alpha": nsWorkloads("alpha", 1, 0),
		"beta":  nsWorkloads("beta", 0),
	}, "")
	m := openModal(t, b, sess)

	// alpha: keep w00 off -> stop, start w01; beta: start w00.
	b.handleRawEvent(submit(t, "u1", m.CustomID, map[string][]string{"0": {"1"}, "1": {"0"}}))
	r := sess.responses[len(sess.responses)-1]
	if r.Type != discordgo.InteractionResponseChannelMessageWithSource || r.Data.Flags != discordgo.MessageFlagsEphemeral {
		t.Fatalf("submit response = %v flags %v, want an ephemeral message", r.Type, r.Data.Flags)
	}
	for _, want := range []string{"alpha, beta change preview:", "**alpha**", "🔴 alpha-w00: running → **stop**", "🟢 alpha-w01: stopped → **start**", "**beta**", "🟢 beta-w00: stopped → **start**"} {
		if !strings.Contains(r.Data.Content, want) {
			t.Errorf("preview lacks %q:\n%s", want, r.Data.Content)
		}
	}

	confirm := r.Data.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.Button)
	b.dispatch(component("u1", confirm.CustomID))
	b.watchers.Wait()
	want := []string{"start:alpha/alpha-w01", "stop:alpha/alpha-w00", "start:beta/beta-w00"}
	if !slices.Equal(store.calls, want) {
		t.Errorf("store calls = %v, want %v", store.calls, want)
	}
	if got := sess.edits[len(sess.edits)-1]; got.Content == nil || !strings.Contains(*got.Content, "(STG / example-staging / alpha, beta)") {
		t.Errorf("result = %v", got.Content)
	}
}

func TestSelectionModal_ForgedSubmissionCannotWidenTheSelection(t *testing.T) {
	t.Parallel()

	const policy = `
version: 1
rules:
  - env: stg
    namespaces: ["alpha"]
    workloads: ["alpha-w00"]
    actions: [start, stop]
    subjects: {discord: {users: ["100000000000000001"]}}
`
	const user = "100000000000000001"
	b, sess, store := modalBot(t, map[string][]*model.Workload{
		"alpha": nsWorkloads("alpha", 0, 0),
		"beta":  nsWorkloads("beta", 0),
	}, policy)
	m := openModalAs(t, b, sess, user)
	if len(m.Components) != 1 || len(m.Components[0].Component.Options) != 1 {
		t.Fatalf("modal = %+v, want only the authorized alpha-w00", m.Components)
	}

	// Out-of-range, non-numeric and unknown-group values are ignored; nothing else can be named.
	b.handleRawEvent(submit(t, user, m.CustomID, map[string][]string{"0": {"1", "x", "-1"}, "9": {"0"}}))
	if got := sess.responses[len(sess.responses)-1].Data.Content; !strings.Contains(got, "no changes") {
		t.Errorf("forged submission content = %q, want no changes", got)
	}

	b.handleRawEvent(submit(t, "u2", m.CustomID, map[string][]string{"0": {"0"}}))
	if got := sess.responses[len(sess.responses)-1].Data.Content; got != msgForeign && got != msgExpired {
		t.Errorf("another user's submission = %q, want a refusal", got)
	}
	if len(store.calls) != 0 {
		t.Errorf("store calls = %v, want none", store.calls)
	}
}

func TestParseSelectionSubmit_IgnoresOtherInteractions(t *testing.T) {
	t.Parallel()

	sel := submit(t, "u1", customID{kindSelectModal, "abc", 0}.String(), map[string][]string{"0": {"1", "2"}})
	i, id, values, ok := parseSelectionSubmit(sel.RawData)
	if !ok || id.Token != "abc" || !slices.Equal(values["0"], []string{"1", "2"}) || i.Member.User.ID != "u1" || i.Token != "tok" || i.AppID != "app" {
		t.Errorf("parsed = %v %+v %v %+v", ok, id, values, i)
	}

	other := submit(t, "u1", customID{kindDurationModal, "abc", 0}.String(), nil)
	if _, _, _, ok := parseSelectionSubmit(other.RawData); ok {
		t.Error("a duration modal must be left to the typed handler")
	}
	if _, _, _, ok := parseSelectionSubmit([]byte(`{"type":3,"data":{"custom_id":"wk1:sm:abc:0"}}`)); ok {
		t.Error("a component interaction must be ignored")
	}
}

func TestMessageFlow_SeveralNamespaces(t *testing.T) {
	t.Parallel()

	byNS := map[string][]*model.Workload{"alpha": nsWorkloads("alpha", 0), "beta": nsWorkloads("beta", 0)}
	b, sess, store := modalBot(t, byNS, "")
	b.noModal = true

	b.dispatch(command("u1", "knock-stg"))
	_, r := lastEdit(t, sess)
	nsMenu := r[0].Components[0].(discordgo.SelectMenu)
	pick := func(ns string) {
		b.dispatch(component("u1", nsMenu.CustomID, ns))
		rows := lastUpdate(t, sess).Components
		menu := rows[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
		b.dispatch(component("u1", menu.CustomID, "0"))
	}
	button := func(label string) {
		t.Helper()
		for _, row := range lastUpdate(t, sess).Components {
			for _, c := range row.(discordgo.ActionsRow).Components {
				if btn, ok := c.(discordgo.Button); ok && btn.Label == label {
					b.dispatch(component("u1", btn.CustomID))
					return
				}
			}
		}
		t.Fatalf("no %q button", label)
	}

	pick("alpha")
	button("Other namespace")
	if got := lastUpdate(t, sess).Content; !strings.Contains(got, "1 namespace(s) selected") {
		t.Errorf("namespace menu = %q, want the selection count", got)
	}
	pick("beta")
	button("Apply")
	if got := lastUpdate(t, sess).Content; !strings.Contains(got, "**alpha**") || !strings.Contains(got, "**beta**") {
		t.Fatalf("preview = %q, want both namespaces", got)
	}
	button("Confirm")
	b.watchers.Wait()
	if want := []string{"start:alpha/alpha-w00", "start:beta/beta-w00"}; !slices.Equal(store.calls, want) {
		t.Errorf("store calls = %v, want %v", store.calls, want)
	}
}
