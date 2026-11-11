package slackbot

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/notify"
	"github.com/murasame29/workload-knocker/internal/reservation"
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

// handleStatus posts the status list to the invoking user, from the in-memory store only.
func (b *Bot) handleStatus(cmd slack.SlashCommand, cl config.Cluster) {
	sub := b.subject(cmd.UserID)
	text, blocks := statusMessage(cl, b.svc.Status(sub, cl.Env), b.visibleReservations(sub, cl.Env), nil)
	if _, err := b.client.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionText(text, false), slack.MsgOptionBlocks(blocks...)); err != nil {
		slog.Error("failed to post status message", slog.String("error.message", err.Error()))
	}
}

// postUsage tells the user which arguments the slash command accepts.
func (b *Bot) postUsage(cmd slack.SlashCommand) {
	msg := fmt.Sprintf("ℹ️ Usage: `%s` opens the start/stop form, `%s %s` lists the running workloads, `%s %s` reserves workloads for a time.", cmd.Command, cmd.Command, slashStatus, cmd.Command, slashSchedule)
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
		b.replaceOriginal(callback, "⚠️ The target Cluster is no longer in the Registry. Please run the command again.", nil)
		return
	}

	sub := b.subject(callback.User.ID)
	var notes []string
	if actionID == actionStatusStop {
		res := b.svc.Apply(frontend.ApplyRequest{Cluster: cl, Namespace: ref.Namespace, User: callback.User.Name, Subject: sub, ToStop: ref.Names})
		notes = res.StopLines()
	} else {
		res := b.svc.Extend(frontend.ExtendRequest{Cluster: cl, Namespace: ref.Namespace, Subject: sub, Names: ref.Names, Duration: notify.ExtendBy})
		notes = res.Lines(slackTime)
	}
	text, blocks := statusMessage(cl, b.svc.Status(sub, cl.Env), b.visibleReservations(sub, cl.Env), notes)
	b.replaceOriginal(callback, text, blocks)
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

// statusMessage renders the status list: the plain-text fallback and the blocks. notes are the
// outcome lines of the action that led to this refresh. Buttons are added per running entry the
// user may operate, as far as Slack's block limit allows.
// The pending reservations follow with a Cancel button each.
func statusMessage(cl config.Cluster, st frontend.Status, pending []reservation.Reservation, notes []string) (string, []slack.Block) {
	head := fmt.Sprintf("*%s / %s* status", strings.ToUpper(cl.Env), cl.Name)
	if st.Empty() {
		lines := append(slices.Clone(notes), "ℹ️ No workloads you may operate are registered in this cluster.")
		return head, mrkdwnSections(head, lines)
	}
	text, blocks := workloadStatus(head, st, notes)
	return text, append(blocks, reservationBlocks(st.Env, pending, slackMessageMaxBlocks-len(blocks))...)
}

// workloadStatus renders the workload part of the status list.
func workloadStatus(head string, st frontend.Status, notes []string) (string, []slack.Block) {
	lines := slices.Clone(notes)
	lines = append(lines, "🟢 running  🟡 partial  🔴 stopped")
	for _, ns := range st.Namespaces {
		lines = append(lines, "*"+escapeMrkdwn(ns.Name)+"*")
		for _, se := range ns.Entries {
			lines = append(lines, statusLine(se))
		}
	}
	blocks := mrkdwnSections(head, lines)

	skipped := 0
	for _, ns := range st.Namespaces {
		for _, se := range ns.Entries {
			row := entryButtons(st.Env, ns.Name, se)
			switch {
			case row == nil:
			case len(blocks) >= slackMessageMaxBlocks-1:
				skipped++
			default:
				blocks = append(blocks, row)
			}
		}
	}
	if skipped > 0 {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn",
			fmt.Sprintf("ℹ️ Slack limits a message to %d blocks; %d entries have no buttons. Use the start/stop form for those.", slackMessageMaxBlocks, skipped), false, false), nil, nil))
	}
	return head, blocks
}

// statusLine renders one entry: its state label, then who started it, when, and its stop time.
func statusLine(se frontend.StatusEntry) string {
	line := escapeMrkdwn(se.Entry.Label())
	if !se.Entry.AnyRunning() {
		return line
	}
	if se.StartedBy != "" {
		line += " · by " + escapeMrkdwn(se.StartedBy)
	}
	if !se.StartedAt.IsZero() {
		line += " · started " + slackTime(se.StartedAt)
	}
	if se.StopAt.IsZero() {
		return line + " · no auto-stop"
	}
	return line + " · stops " + slackTime(se.StopAt)
}

// entryButtons returns the action row of an entry, or nil when the user may do neither action.
func entryButtons(env, namespace string, se frontend.StatusEntry) slack.Block {
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
	add(actionStatusExtend, "Extend 1h", se.ExtendNames, false)
	add(actionStatusStop, "Stop", se.StopNames, true)
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
