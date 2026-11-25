package slackbot

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/frontend/notify"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
)

const (
	// Action IDs of the buttons of a status entry.
	actionStatusExtend = "status_extend"
	actionStatusStop   = "status_stop"

	// slackMessageMaxBlocks is Slack's cap of blocks per message.
	slackMessageMaxBlocks = 50
	// slackButtonTextMaxLen / slackButtonValueMaxLen are Slack's caps of button text and value.
	slackButtonTextMaxLen  = 75
	slackButtonValueMaxLen = 2000

	slashStatus = "status"
)

// slashArg is the parsed text of a slash command.
type slashArg int

const (
	argModal    slashArg = iota // no argument: the start/stop modal
	argStatus                   // "status"
	argSchedule                 // "schedule"
	argUnknown                  // anything else
)

// parseSlashArg reads the text typed after the slash command.
func parseSlashArg(text string) slashArg {
	fields := strings.Fields(text)
	switch {
	case len(fields) == 0:
		return argModal
	case len(fields) == 1 && strings.EqualFold(fields[0], slashStatus):
		return argStatus
	case len(fields) == 1 && strings.EqualFold(fields[0], slashSchedule):
		return argSchedule
	}
	return argUnknown
}

// handleStatus opens the status flow at its namespace select.
func (b *Bot) handleStatus(cmd slack.SlashCommand, cl config.Cluster) {
	b.openView(cmd.TriggerID, b.statusStart(b.subject(cmd.UserID), cl, cmd.ChannelID, cmd.UserID))
}

// postUsage tells the user which arguments the slash command accepts.
func (b *Bot) postUsage(cmd slack.SlashCommand) {
	msg := b.t("ℹ️ Usage: `%[1]s` opens the menu (start/stop, schedule, status), `%[1]s %[2]s` lists the workloads of a namespace, `%[1]s %[3]s` reserves workloads for later.", cmd.Command, slashStatus, slashSchedule)
	if _, err := b.client.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionText(msg, false)); err != nil {
		slog.Error("failed to post ephemeral message", slog.String("error.message", err.Error()))
	}
}

// handleStatusAction runs the Extend or Stop button of a status entry and replaces the list in place.
func (b *Bot) handleStatusAction(callback slack.InteractionCallback, actionID, payload string) {
	ref, err := frontend.DecodeStatusRef(payload)
	if err != nil {
		slog.Warn("invalid status button payload", slog.String("error.message", err.Error()))
		return
	}
	cl, ok := b.registry.ByEnv(ref.Env)
	if !ok {
		b.replaceOriginal(callback, b.t("⚠️ The target Cluster is no longer in the Registry. Please run the command again."), nil)
		return
	}

	sub := b.subject(callback.User.ID)
	var notes []string
	if actionID == actionStatusStop {
		res := b.svc.Apply(frontend.ApplyRequest{Cluster: cl, Namespace: ref.Namespace, User: callback.User.Name, Subject: sub, ToStop: ref.Names})
		notes = b.markup.StopLines(res)
	} else {
		res := b.svc.Extend(frontend.ExtendRequest{Cluster: cl, Namespace: ref.Namespace, Subject: sub, Names: ref.Names, Duration: notify.ExtendBy})
		notes = b.markup.ExtendLines(res, slackTime)
	}
	b.refreshStatus(callback, cl, sub, notes)
}

// replaceOriginal replaces the ephemeral message the button was pressed on.
func (b *Bot) replaceOriginal(callback slack.InteractionCallback, text string, blocks []slack.Block) {
	msg := &slack.WebhookMessage{Text: text, ResponseType: slack.ResponseTypeEphemeral, ReplaceOriginal: true}
	if blocks != nil {
		msg.Blocks = &slack.Blocks{BlockSet: blocks}
	}
	ctx, cancel := context.WithTimeout(context.Background(), responseTimeout)
	defer cancel()
	if err := b.webhook(ctx, callback.ResponseURL, msg); err != nil {
		slog.Error("failed to replace status message", slog.String("error.message", err.Error()))
	}
}

// responseTimeout bounds the call to a response_url.
const responseTimeout = 5 * time.Second

// statusMessage renders the status list as a message (at most slackMessageMaxBlocks blocks).
func (u ui) statusMessage(cl config.Cluster, st frontend.Status, pending []reservation.Reservation, notes []string) (string, []slack.Block) {
	return u.statusBlocks(cl, st, pending, notes, slackMessageMaxBlocks)
}

// statusBlocks renders the status list: the plain-text fallback and at most maxBlocks blocks.
// notes are the outcome lines of the action that led to this refresh. Buttons are added per
// running entry the user may operate, as far as the block limit allows.
// The pending reservations follow with a Cancel button each.
func (u ui) statusBlocks(cl config.Cluster, st frontend.Status, pending []reservation.Reservation, notes []string, maxBlocks int) (string, []slack.Block) {
	head := u.t("*%s / %s* status", strings.ToUpper(cl.Env), cl.Name)
	if st.Empty() {
		lines := append(slices.Clone(notes), u.t("ℹ️ No workloads you may operate are registered in this cluster."))
		return head, mrkdwnSections(head, lines)
	}
	text, blocks := u.workloadStatus(head, st, notes, maxBlocks)
	return text, append(blocks, u.reservationBlocks(st.Env, pending, maxBlocks-len(blocks))...)
}

// workloadStatus renders the workload part of the status list.
func (u ui) workloadStatus(head string, st frontend.Status, notes []string, maxBlocks int) (string, []slack.Block) {
	lines := slices.Clone(notes)
	lines = append(lines, u.t("🟢 running  🟡 partial  🔴 stopped"))
	for _, ns := range st.Namespaces {
		lines = append(lines, "*"+escapeMrkdwn(ns.Name)+"*")
		for _, se := range ns.Entries {
			lines = append(lines, u.markup.StatusLine(se))
		}
	}
	blocks := mrkdwnSections(head, lines)

	skipped := 0
	for _, ns := range st.Namespaces {
		for _, se := range ns.Entries {
			row := u.entryButtons(st.Env, ns.Name, se)
			switch {
			case row == nil:
			case len(blocks) >= maxBlocks-1:
				skipped++
			default:
				blocks = append(blocks, row)
			}
		}
	}
	if skipped > 0 {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn",
			u.t("ℹ️ Slack limits this view to %d blocks; %d entries have no buttons. Use the start/stop form for those.", maxBlocks, skipped), false, false), nil, nil))
	}
	return head, blocks
}

// entryButtons returns the action row of an entry, or nil when the user may do neither action.
func (u ui) entryButtons(env, namespace string, se frontend.StatusEntry) slack.Block {
	var elems []slack.BlockElement
	add := func(actionID, label string, names []string, danger bool) {
		if len(names) == 0 {
			return
		}
		value := frontend.EncodeStatusRef(frontend.StatusRef{Env: env, Namespace: namespace, Names: names})
		if len(value) > slackButtonValueMaxLen {
			return
		}
		btn := slack.NewButtonBlockElement(actionID, value, slack.NewTextBlockObject("plain_text", truncateText(label+": "+se.Entry.Key, slackButtonTextMaxLen), false, false))
		if danger {
			btn = btn.WithStyle(slack.StyleDanger)
		}
		elems = append(elems, btn)
	}
	add(actionStatusExtend, u.t("Extend 1h"), se.ExtendNames, false)
	add(actionStatusStop, u.t("Stop"), se.StopNames, true)
	if len(elems) == 0 {
		return nil
	}
	return slack.NewActionBlock("", elems...)
}

func truncateText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// escapeMrkdwn escapes the characters Slack treats as control characters in mrkdwn text.
func escapeMrkdwn(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
