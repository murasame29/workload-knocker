package discordbot

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
)

const (
	roleDevs  = "223456789012345678"
	roleOther = "323456789012345678"
	// policyBatchOnly lets the devs role operate only the batch workload of labs.
	policyBatchOnly = `
version: 1
default: deny
rules:
  - env: stg
    namespaces: [labs]
    workloads: [batch]
    subjects:
      discord:
        roles: ["` + roleDevs + `"]
`
)

func memberWithRoles(id string, roles ...string) *discordgo.Member {
	return &discordgo.Member{User: &discordgo.User{ID: id}, Roles: roles}
}

func commandWithRoles(user, name string, roles ...string) *discordgo.Interaction {
	i := command(user, name)
	i.Member = memberWithRoles(user, roles...)
	return i
}

func componentWithRoles(user, customID string, roles []string, values ...string) *discordgo.Interaction {
	i := component(user, customID, values...)
	i.Member = memberWithRoles(user, roles...)
	return i
}

func TestSubject_UserAndRoles(t *testing.T) {
	t.Parallel()

	got := subject(&discordgo.Interaction{Member: memberWithRoles("u1", roleDevs, roleOther)})
	want := authz.Subject{Platform: authz.PlatformDiscord, User: "u1", Groups: []string{roleDevs, roleOther}}
	if got.Platform != want.Platform || got.User != want.User || !slices.Equal(got.Groups, want.Groups) {
		t.Errorf("subject = %+v, want %+v", got, want)
	}

	dm := subject(&discordgo.Interaction{User: &discordgo.User{ID: "d1"}})
	if dm.Platform != authz.PlatformDiscord || dm.User != "d1" || len(dm.Groups) != 0 {
		t.Errorf("DM subject = %+v", dm)
	}

	// The subject must not alias the interaction's role slice.
	m := memberWithRoles("u1", roleDevs)
	s := subject(&discordgo.Interaction{Member: m})
	s.Groups[0] = "x"
	if m.Roles[0] != roleDevs {
		t.Error("subject shares the member's role slice")
	}
}

func TestFlow_PolicyFiltersListing(t *testing.T) {
	t.Parallel()

	ws := []*model.Workload{wl("batch", 0), wl("secret", 0)}
	b, sess, _ := newTestBotWith(t, ws, policyBatchOnly, mode.Full)

	// A user without the role has nothing to operate.
	b.dispatch(commandWithRoles("u1", "knock-stg", roleOther))
	if c, _ := lastEdit(t, sess); c != msgNotAuthorized {
		t.Errorf("unauthorized content = %q", c)
	}

	// A user with the role sees only the permitted workload.
	b.dispatch(commandWithRoles("u2", "knock-stg", roleDevs))
	_, r := lastEdit(t, sess)
	nsMenu := r[0].Components[0].(discordgo.SelectMenu)
	b.dispatch(componentWithRoles("u2", nsMenu.CustomID, []string{roleDevs}, "labs"))
	menu := lastUpdate(t, sess).Components[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	if len(menu.Options) != 1 || !strings.Contains(menu.Options[0].Label, "batch") {
		t.Fatalf("options = %+v, want only batch", menu.Options)
	}
}

func TestFlow_RolesRevokedBeforeApplyAreRechecked(t *testing.T) {
	t.Parallel()

	b, sess, store := newTestBotWith(t, []*model.Workload{wl("batch", 0)}, policyBatchOnly, mode.Full)
	b.dispatch(commandWithRoles("u1", "knock-stg", roleDevs))
	_, r := lastEdit(t, sess)
	nsMenu := r[0].Components[0].(discordgo.SelectMenu)
	devs := []string{roleDevs}
	b.dispatch(componentWithRoles("u1", nsMenu.CustomID, devs, "labs"))
	wlMenu := lastUpdate(t, sess).Components[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	b.dispatch(componentWithRoles("u1", wlMenu.CustomID, devs, "0"))
	apply := lastUpdate(t, sess).Components[1].(discordgo.ActionsRow).Components[0].(discordgo.Button).CustomID
	b.dispatch(componentWithRoles("u1", apply, devs))
	var confirm string
	for _, c := range lastUpdate(t, sess).Components[0].(discordgo.ActionsRow).Components {
		if btn := c.(discordgo.Button); btn.Label == "Confirm" {
			confirm = btn.CustomID
		}
	}

	// The role is gone by the time Confirm is clicked: nothing may run.
	b.dispatch(componentWithRoles("u1", confirm, []string{roleOther}))
	if len(store.calls) != 0 {
		t.Fatalf("store calls after revoked role = %v", store.calls)
	}
	if got := *sess.edits[len(sess.edits)-1].Content; !strings.Contains(got, "⛔ batch: you are not authorized to start this") {
		t.Errorf("result = %q", got)
	}
}

func TestApply_IgnoresForgedFlowContents(t *testing.T) {
	t.Parallel()

	b, sess, store := newTestBotWith(t, []*model.Workload{wl("batch", 0), wl("secret", 0)}, policyBatchOnly, mode.Full)
	// A flow whose stored plan names a workload the policy does not allow.
	token, err := b.flows.create(&flow{userID: "u1", env: "stg", namespace: "labs", toStart: []string{"batch", "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	b.dispatch(componentWithRoles("u1", customID{kindConfirm, token, 0}.String(), []string{roleDevs}))
	if !slices.Equal(store.calls, []string{"start:batch:u1"}) {
		t.Errorf("store calls = %v, want only the permitted start", store.calls)
	}
	if got := *sess.edits[len(sess.edits)-1].Content; !strings.Contains(got, "⛔ secret") {
		t.Errorf("result = %q lacks the rejection", got)
	}
}

func TestFlow_ScheduledStopRejectsStop(t *testing.T) {
	t.Parallel()

	b, sess, store := newTestBotWith(t, []*model.Workload{wl("legacy", 1), wl("batch", 0)}, "", mode.ScheduledStop)
	b.dispatch(command("u1", "knock-stg"))
	_, r := lastEdit(t, sess)
	nsMenu := r[0].Components[0].(discordgo.SelectMenu)
	b.dispatch(component("u1", nsMenu.CustomID, "labs"))
	v := lastUpdate(t, sess)
	if !strings.Contains(v.Content, "Stopping from chat is disabled") {
		t.Errorf("workload view lacks the stop-disabled note:\n%s", v.Content)
	}

	// Deselect legacy (stop) only: the plan is empty and explains why.
	wlMenu := v.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	b.dispatch(component("u1", wlMenu.CustomID))
	apply := lastUpdate(t, sess).Components[1].(discordgo.ActionsRow).Components[0].(discordgo.Button).CustomID
	b.dispatch(component("u1", apply))
	got := lastUpdate(t, sess).Content
	if !strings.Contains(got, "⛔ legacy: "+(frontend.Rejection{Reason: frontend.ReasonChatStopDisabled}).Message()) {
		t.Errorf("preview = %q", got)
	}
	if len(store.calls) != 0 {
		t.Errorf("store calls = %v, want none", store.calls)
	}
}

func TestFlow_ScheduledStopStillStarts(t *testing.T) {
	t.Parallel()

	b, sess, store := newTestBotWith(t, []*model.Workload{wl("legacy", 1), wl("batch", 0)}, "", mode.ScheduledStop)
	b.dispatch(command("u1", "knock-stg"))
	_, r := lastEdit(t, sess)
	nsMenu := r[0].Components[0].(discordgo.SelectMenu)
	b.dispatch(component("u1", nsMenu.CustomID, "labs"))
	wlMenu := lastUpdate(t, sess).Components[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	// Keep legacy (index 1), add batch (index 0).
	b.dispatch(component("u1", wlMenu.CustomID, "0", "1"))
	apply := lastUpdate(t, sess).Components[1].(discordgo.ActionsRow).Components[0].(discordgo.Button).CustomID
	b.dispatch(component("u1", apply))
	var confirm string
	for _, c := range lastUpdate(t, sess).Components[0].(discordgo.ActionsRow).Components {
		if btn := c.(discordgo.Button); btn.Label == "Confirm" {
			confirm = btn.CustomID
		}
	}
	b.dispatch(component("u1", confirm))
	if !slices.Equal(store.calls, []string{"start:batch:u1"}) {
		t.Errorf("store calls = %v", store.calls)
	}
}

func TestApply_ScheduledStopRejectsForgedStop(t *testing.T) {
	t.Parallel()

	b, sess, store := newTestBotWith(t, []*model.Workload{wl("legacy", 1)}, "", mode.ScheduledStop)
	token, _ := b.flows.create(&flow{userID: "u1", env: "stg", namespace: "labs", toStop: []string{"legacy"}})
	b.dispatch(component("u1", customID{kindConfirm, token, 0}.String()))
	if len(store.calls) != 0 {
		t.Errorf("store calls = %v, want none", store.calls)
	}
	if got := *sess.edits[len(sess.edits)-1].Content; !strings.Contains(got, "OPERATION_MODE=scheduled-stop") {
		t.Errorf("result = %q", got)
	}
}

func TestWorkloadView_StopDisabledNote(t *testing.T) {
	t.Parallel()

	entries := frontend.GroupEntries([]*model.Workload{wl("a", 1)})
	if c := workloadView("t", testCluster(), "labs", entries, nil, 0, false).Content; !strings.Contains(c, "Stopping from chat is disabled") {
		t.Errorf("missing note: %q", c)
	}
	if c := workloadView("t", testCluster(), "labs", entries, nil, 0, true).Content; strings.Contains(c, "disabled") {
		t.Errorf("unexpected note: %q", c)
	}
}

func discordCluster(env, cmd string) config.Cluster {
	cl := testCluster()
	cl.Env, cl.Name = env, env
	cl.Frontends.Discord = &config.DiscordFrontend{Command: cmd}
	return cl
}

func validated(t *testing.T, cls ...config.Cluster) *config.Config {
	t.Helper()
	c := &config.Config{Controller: config.Controller{Namespace: "wk"}, Clusters: cls}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReload_ReRegistersChangedCommandSet(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, nil)
	live := b.registry.(*config.Holder)
	if err := b.registerCommands(); err != nil {
		t.Fatal(err)
	}
	if sess.overwrites != 1 {
		t.Fatalf("overwrites = %d", sess.overwrites)
	}

	b.syncCommands()
	if sess.overwrites != 1 {
		t.Errorf("unchanged command set re-registered: %d overwrites", sess.overwrites)
	}

	live.Store(validated(t, discordCluster("stg", "knock-stg"), discordCluster("dev", "knock-dev")))
	b.syncCommands()
	if sess.overwrites != 2 {
		t.Fatalf("overwrites = %d, want a re-registration", sess.overwrites)
	}
	var names []string
	for _, c := range sess.overwrote {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"knock-dev", "knock-stg"}) || sess.guild != "guild-1" {
		t.Errorf("registered %v in guild %q", names, sess.guild)
	}

	live.Store(validated(t, discordCluster("dev", "knock-dev")))
	b.syncCommands()
	if len(sess.overwrote) != 1 || sess.overwrote[0].Name != "knock-dev" {
		t.Errorf("removed command still registered: %+v", sess.overwrote)
	}
}

func TestReload_ReRegistrationFailureRetries(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, nil)
	live := b.registry.(*config.Holder)
	if err := b.registerCommands(); err != nil {
		t.Fatal(err)
	}
	live.Store(validated(t, discordCluster("stg", "knock-new")))

	sess.overwriteErr = errors.New("boom")
	b.syncCommands()
	if !slices.Equal(b.registered, []string{"knock-stg"}) {
		t.Fatalf("registered = %v, a failed sync must keep the old set", b.registered)
	}

	sess.overwriteErr = nil
	b.syncCommands()
	if !slices.Equal(b.registered, []string{"knock-new"}) {
		t.Errorf("registered = %v, want retry to succeed", b.registered)
	}
}

func TestReload_CommandsResolveAgainstLiveRegistry(t *testing.T) {
	t.Parallel()

	b, sess, _ := newTestBot(t, []*model.Workload{wl("batch", 0)})
	live := b.registry.(*config.Holder)

	live.Store(validated(t, discordCluster("stg", "knock-renamed")))
	b.dispatch(command("u1", "knock-stg"))
	if got := sess.responses[len(sess.responses)-1].Data; got == nil || !strings.Contains(got.Content, "not registered") {
		t.Errorf("old command still resolves: %+v", sess.responses[len(sess.responses)-1])
	}
	b.dispatch(command("u1", "knock-renamed"))
	if len(sess.edits) == 0 {
		t.Error("renamed command did not resolve after reload")
	}
}
