package discordbot

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
)

const (
	msgExpired       = "⏱ This menu expired. Run the command again."
	msgForeign       = "⚠ This menu belongs to another user."
	msgClusterGone   = "⚠ The target Cluster is no longer in the Registry. Please run the command again."
	msgUnrecognized  = "⚠ Unrecognized control. Run the command again."
	msgNotAuthorized = "⚠ You are not authorized to operate any workload in this cluster."
)

// interactionUser returns the invoking user (Member.User in a guild, User in a DM).
func interactionUser(i *discordgo.Interaction) (id, name string) {
	switch {
	case i.Member != nil && i.Member.User != nil:
		return i.Member.User.ID, i.Member.User.Username
	case i.User != nil:
		return i.User.ID, i.User.Username
	}
	return "", ""
}

func (b *Bot) respond(i *discordgo.Interaction, r *discordgo.InteractionResponse) bool {
	if err := b.sess.Respond(i, r); err != nil {
		slog.Error("discord interaction respond failed", slog.String("error.message", err.Error()))
		return false
	}
	return true
}

// noMentions stops user mentions in bot text from pinging anyone.
func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

func (b *Bot) deferEphemeral(i *discordgo.Interaction) bool {
	return b.respond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
}

func (b *Bot) deferUpdate(i *discordgo.Interaction) bool {
	return b.respond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
}

func (b *Bot) replyEphemeral(i *discordgo.Interaction, msg string) {
	b.respond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: msg, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions()},
	})
}

// update replaces the component's message in place (type 7, answers within the 3s window).
func (b *Bot) update(i *discordgo.Interaction, v view) {
	b.respond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: v.Content, Components: v.Components, AllowedMentions: noMentions()},
	})
}

// edit replaces the message after a deferred response.
func (b *Bot) edit(i *discordgo.Interaction, v view) {
	content, comps := v.Content, v.Components
	if comps == nil {
		comps = noComponents()
	}
	if err := b.sess.Edit(i, &discordgo.WebhookEdit{Content: &content, Components: &comps, AllowedMentions: noMentions()}); err != nil {
		slog.Error("discord interaction edit failed", slog.String("error.message", err.Error()))
	}
}

// handleCommand handles the configured slash commands: it shows the namespace menu.
func (b *Bot) handleCommand(i *discordgo.Interaction) {
	data, ok := i.Data.(discordgo.ApplicationCommandInteractionData)
	if !ok {
		slog.Warn("discord command data type assertion failed")
		return
	}
	cl, ok := b.registry.ByDiscordCommand(data.Name)
	if !ok {
		slog.Warn("unknown discord command", slog.String("command", data.Name))
		b.replyEphemeral(i, "⚠ This command is not registered in the Cluster Registry")
		return
	}
	userID, userName := interactionUser(i)
	if userID == "" {
		slog.Warn("discord command without a user", slog.String("command", data.Name))
		return
	}

	slog.Info("slash command received", slog.String("user.id", userID), slog.String("user.name", userName), slog.String("command", data.Name), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env))

	// Ack first: the on-demand Reconcile can take longer than the 3s response window.
	if !b.deferEphemeral(i) {
		return
	}
	b.svc.Reconcile(cl.Env)

	namespaces, total := b.svc.Namespaces(subject(i), cl.Env)
	if len(namespaces) == 0 {
		if total > 0 {
			b.edit(i, text(msgNotAuthorized))
		} else {
			b.edit(i, text("⚠ No managed workloads are registered yet. Please wait for ScaledObject discovery."))
		}
		return
	}
	token, err := b.flows.create(&flow{userID: userID, env: cl.Env})
	if err != nil {
		slog.Error("failed to create discord flow", slog.String("error.message", err.Error()))
		b.edit(i, text("⚠ Internal error. Please try again."))
		return
	}
	b.edit(i, namespaceView(token, cl, namespaces, 0))
}

// handleComponent handles select menus and buttons of an open flow.
func (b *Bot) handleComponent(i *discordgo.Interaction) {
	data, ok := i.Data.(discordgo.MessageComponentInteractionData)
	if !ok {
		slog.Warn("discord component data type assertion failed")
		return
	}
	id, err := parseCustomID(data.CustomID)
	userID, _ := interactionUser(i)
	if err != nil || userID == "" {
		slog.Warn("unrecognized discord component", slog.String("custom_id", data.CustomID))
		b.replyEphemeral(i, msgUnrecognized)
		return
	}

	switch id.Kind {
	case kindConfirm:
		b.handleConfirm(i, id, userID)
	case kindNamespacePage, kindNamespaceSelect, kindWorkloadPage, kindWorkloadSelect, kindPlan, kindCancel:
		b.withFlow(i, id, userID, func(f *flow, cl config.Cluster) view {
			return b.step(id, data.Values, subject(i), f, cl)
		})
	default:
		slog.Warn("unknown discord component kind", slog.String("kind", id.Kind))
		b.replyEphemeral(i, msgUnrecognized)
	}
}

// withFlow runs fn on the invoker's live flow and updates the message with its view.
func (b *Bot) withFlow(i *discordgo.Interaction, id customID, userID string, fn func(*flow, config.Cluster) view) {
	var v view
	denied := false
	ok := b.flows.with(id.Token, func(f *flow) {
		if f.userID != userID {
			denied = true
			return
		}
		cl, found := b.registry.ByEnv(f.env)
		if !found {
			f.expires = time.Time{}
			v = text(msgClusterGone)
			return
		}
		v = fn(f, cl)
	})
	switch {
	case denied:
		b.replyEphemeral(i, msgForeign)
	case !ok:
		b.update(i, text(msgExpired))
	default:
		b.update(i, v)
	}
}

// step advances the flow for one non-confirm component and returns the next view.
func (b *Bot) step(id customID, values []string, sub authz.Subject, f *flow, cl config.Cluster) view {
	token := id.Token
	switch id.Kind {
	case kindNamespacePage:
		f.nsPage = id.Arg
		namespaces, _ := b.svc.Namespaces(sub, cl.Env)
		return b.namespaces(token, cl, namespaces, f.nsPage)

	case kindNamespaceSelect:
		namespaces, _ := b.svc.Namespaces(sub, cl.Env)
		if len(values) != 1 || !slices.Contains(namespaces, values[0]) {
			return b.namespaces(token, cl, namespaces, f.nsPage)
		}
		return b.selectNamespace(token, sub, f, cl, values[0])

	case kindWorkloadPage:
		f.page = id.Arg
		return b.workloads(token, cl, f)

	case kindWorkloadSelect:
		applyPageSelection(f, values)
		return b.workloads(token, cl, f)

	case kindPlan:
		return b.previewPlan(token, sub, f, cl)

	default: // kindCancel
		f.expires = time.Time{}
		return text("Cancelled.")
	}
}

// selectNamespace snapshots the namespace's entries; running entries start selected so an
// unchanged plan does not stop them.
func (b *Bot) selectNamespace(token string, sub authz.Subject, f *flow, cl config.Cluster, namespace string) view {
	slog.Info("namespace selected", slog.String("user.id", f.userID), slog.String("cluster.name", cl.Name), slog.String("cluster.env", cl.Env), slog.String("namespace", namespace))

	listing := b.svc.List(sub, cl.Env, namespace, frontend.Limits{PageSize: selectMaxOptions})
	f.namespace = namespace
	f.page = 0
	f.entries = listing.Entries
	f.selected = make(map[string]bool, len(listing.Entries))
	for _, e := range listing.Entries {
		if e.AnyRunning() {
			f.selected[e.Value()] = true
		}
	}
	return b.workloads(token, cl, f)
}

// namespaces renders the namespace menu, or the not-authorized notice when the policy leaves none.
func (b *Bot) namespaces(token string, cl config.Cluster, namespaces []string, page int) view {
	if len(namespaces) == 0 {
		return text(msgNotAuthorized)
	}
	return namespaceView(token, cl, namespaces, page)
}

func (b *Bot) workloads(token string, cl config.Cluster, f *flow) view {
	return workloadView(token, cl, f.namespace, f.entries, f.selected, f.page, b.svc.ChatStopAllowed())
}

// applyPageSelection folds the values submitted by the visible page's menu into f.selected.
// Entries on other pages keep their state. Values are entry indexes; out-of-page ones are ignored.
func applyPageSelection(f *flow, values []string) {
	_, start, end, _ := pageBounds(len(f.entries), selectMaxOptions, f.page)
	for i := start; i < end; i++ {
		delete(f.selected, f.entries[i].Value())
	}
	for _, v := range values {
		idx, err := strconv.Atoi(v)
		if err != nil || idx < start || idx >= end {
			continue
		}
		f.selected[f.entries[idx].Value()] = true
	}
}

// buildPlan diffs the selection against the current state. Only snapshot entries take part,
// so workloads that appeared after the menu was rendered are never changed by accident.
func (b *Bot) buildPlan(sub authz.Subject, cl config.Cluster, f *flow) frontend.Plan {
	shown := make(map[string]bool, len(f.entries))
	for _, e := range f.entries {
		shown[e.Value()] = true
	}
	listing := b.svc.List(sub, cl.Env, f.namespace, frontend.Limits{})
	var entries []frontend.Entry
	for _, e := range listing.Entries {
		if shown[e.Value()] {
			entries = append(entries, e)
		}
	}
	return b.svc.Plan(sub, cl, f.namespace, entries, f.selected, f.requestID)
}

func (b *Bot) previewPlan(token string, sub authz.Subject, f *flow, cl config.Cluster) view {
	f.requestID = audit.NewRequestID()
	plan := b.buildPlan(sub, cl, f)
	if plan.Empty() {
		f.expires = time.Time{}
		head := fmt.Sprintf("ℹ %s / %s — no changes", header(cl), f.namespace)
		return text(fitLines(head, rejectionLines(plan.Rejected), contentMaxLen))
	}
	f.toStart, f.toStop = plan.ToStart, plan.ToStop
	return previewView(token, cl, f.namespace, plan.Changes, plan.Rejected)
}

// handleConfirm applies the previewed plan once; the flow is consumed before applying so a
// double click cannot run it twice.
func (b *Bot) handleConfirm(i *discordgo.Interaction, id customID, userID string) {
	var req frontend.ApplyRequest
	var cl config.Cluster
	problem := ""
	sub := subject(i)
	ok := b.flows.with(id.Token, func(f *flow) {
		c, found := b.registry.ByEnv(f.env)
		switch {
		case f.userID != userID:
			problem = msgForeign
		case !found:
			problem = msgClusterGone
			f.expires = time.Time{}
		case len(f.toStart)+len(f.toStop) == 0:
			problem = msgUnrecognized
		default:
			cl = c
			req = frontend.ApplyRequest{Cluster: c, Namespace: f.namespace, User: userID, Subject: sub, RequestID: f.requestID, ToStart: f.toStart, ToStop: f.toStop}
			f.expires = time.Time{}
		}
	})
	switch {
	case !ok:
		b.update(i, text(msgExpired))
		return
	case problem == msgForeign:
		b.replyEphemeral(i, problem)
		return
	case problem != "":
		b.update(i, text(problem))
		return
	}

	// Ack first: applying calls the managed cluster API.
	if !b.deferUpdate(i) {
		return
	}
	res := b.svc.Apply(req)
	b.edit(i, text(resultText(userID, cl, req.Namespace, res)))
}
