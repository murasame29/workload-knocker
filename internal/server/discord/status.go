package discordbot

import (
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/notify"
)

// Subcommands of the registered slash command.
const (
	subcommandStart  = "start"
	subcommandStatus = "status"
)

const (
	// maxMessageButtons is Discord's cap of buttons per message (5 rows of 5).
	maxMessageButtons = 25
	buttonsPerRow     = 5
	buttonLabelMaxLen = 80
)

var snowflakeRe = regexp.MustCompile(`^[0-9]{17,20}$`)

// subcommandOf returns the invoked subcommand; "" for a bare command (clients with a cached command shape).
func subcommandOf(data discordgo.ApplicationCommandInteractionData) string {
	if len(data.Options) > 0 && data.Options[0].Type == discordgo.ApplicationCommandOptionSubCommand {
		return data.Options[0].Name
	}
	return ""
}

// handleStatus answers /<command> status from the in-memory store, inside the 3s window.
func (b *Bot) handleStatus(i *discordgo.Interaction, cl config.Cluster) {
	sub := subject(i)
	v := b.statusView(cl, sub, b.svc.Status(sub, cl.Env), nil)
	b.respond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: v.Content, Components: v.Components, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions()},
	})
}

// handleStatusAction runs the Extend or Stop button of a status entry and refreshes the list in place.
func (b *Bot) handleStatusAction(i *discordgo.Interaction, id customID, userID string) {
	payload, ok := b.poster.buttons.get(id.Token)
	if !ok {
		b.replyEphemeral(i, msgExpired)
		return
	}
	ref, err := frontend.DecodeStatusRef(payload)
	if err != nil {
		slog.Warn("invalid status button payload", slog.String("error.message", err.Error()))
		b.replyEphemeral(i, msgUnrecognized)
		return
	}
	cl, found := b.registry.ByEnv(ref.Env)
	if !found {
		b.update(i, text(msgClusterGone))
		return
	}
	// Ack first: extending and stopping persist state through the cluster API.
	if !b.deferUpdate(i) {
		return
	}

	sub := subject(i)
	var notes []string
	if id.Kind == kindStatusStop {
		res := b.svc.Apply(frontend.ApplyRequest{Cluster: cl, Namespace: ref.Namespace, User: userID, Subject: sub, ToStop: ref.Names})
		notes = res.StopLines()
	} else {
		res := b.svc.Extend(frontend.ExtendRequest{Cluster: cl, Namespace: ref.Namespace, Subject: sub, Names: ref.Names, Duration: notify.ExtendBy})
		notes = res.Lines(stopTime)
	}
	b.edit(i, b.statusView(cl, sub, b.svc.Status(sub, cl.Env), notes))
}

// statusView renders the status list with a button pair per running entry the user may operate,
// then the pending reservations sub may cancel. notes are the outcome lines of the action that
// led to this refresh.
func (b *Bot) statusView(cl config.Cluster, sub authz.Subject, st frontend.Status, notes []string) view {
	head := header(cl) + " status"
	if st.Empty() {
		return text(fitLines(head, append(slices.Clone(notes), "ℹ No workloads you may operate are registered in this cluster."), contentMaxLen))
	}
	v := b.workloadStatus(cl, st, notes)
	lines, cancels := b.reservationLines(sub, st.Env, maxMessageButtons-countButtons(v.Components))
	if len(lines) == 0 {
		return v
	}
	var buttons []discordgo.MessageComponent
	for _, row := range v.Components {
		if r, ok := row.(discordgo.ActionsRow); ok {
			buttons = append(buttons, r.Components...)
		}
	}
	return view{Content: fitLines(v.Content, lines, contentMaxLen), Components: buttonRows(append(buttons, cancels...))}
}

func countButtons(rows []discordgo.MessageComponent) int {
	n := 0
	for _, row := range rows {
		if r, ok := row.(discordgo.ActionsRow); ok {
			n += len(r.Components)
		}
	}
	return n
}

// workloadStatus renders the workload part of the status list.
func (b *Bot) workloadStatus(cl config.Cluster, st frontend.Status, notes []string) view {
	head := header(cl) + " status"
	var body []string
	var buttons []discordgo.MessageComponent
	skipped := 0
	for _, ns := range st.Namespaces {
		body = append(body, "**"+ns.Name+"**")
		for _, se := range ns.Entries {
			body = append(body, statusLine(se))
			pair := b.entryButtons(st.Env, ns.Name, se)
			switch {
			case len(pair) == 0:
			case len(buttons)+len(pair) > maxMessageButtons:
				skipped++
			default:
				buttons = append(buttons, pair...)
			}
		}
	}
	lines := append(slices.Clone(notes), "🟢 running  🟡 partial  🔴 stopped")
	if skipped > 0 {
		lines = append(lines, fmt.Sprintf("ℹ Discord allows %d buttons per message; %d entries have none. Use the start subcommand for those.", maxMessageButtons, skipped))
	}
	lines = append(lines, body...)
	return view{Content: fitLines(head, lines, contentMaxLen), Components: buttonRows(buttons)}
}

// statusLine renders one entry: its state label, then who started it, when, and its stop time.
func statusLine(se frontend.StatusEntry) string {
	line := se.Entry.Label()
	if !se.Entry.AnyRunning() {
		return line
	}
	if se.StartedBy != "" {
		line += " · by " + displayStarter(se.StartedBy)
	}
	if !se.StartedAt.IsZero() {
		line += " · started " + relativeTime(se.StartedAt)
	}
	if se.StopAt.IsZero() {
		return line + " · no auto-stop"
	}
	return line + " · stops " + relativeTime(se.StopAt)
}

// displayStarter mentions Discord user IDs (the recorded starter of Discord starts); other values are shown as is.
func displayStarter(s string) string {
	if snowflakeRe.MatchString(s) {
		return "<@" + s + ">"
	}
	return s
}

func relativeTime(t time.Time) string { return fmt.Sprintf("<t:%d:R>", t.Unix()) }

// entryButtons returns the Extend 1h and Stop buttons the user may press for the entry.
func (b *Bot) entryButtons(env, namespace string, se frontend.StatusEntry) []discordgo.MessageComponent {
	var out []discordgo.MessageComponent
	add := func(kind, label string, style discordgo.ButtonStyle, names []string) {
		if len(names) == 0 {
			return
		}
		token, err := b.poster.buttons.put(frontend.EncodeStatusRef(frontend.StatusRef{Env: env, Namespace: namespace, Names: names}))
		if err != nil {
			slog.Error("failed to create status button", slog.String("error.message", err.Error()))
			return
		}
		out = append(out, button(truncateRunes(label+": "+se.Entry.Key, buttonLabelMaxLen), style, customID{kind, token, 0}, false))
	}
	add(kindStatusExtend, "Extend 1h", discordgo.PrimaryButton, se.ExtendNames)
	add(kindStatusStop, "Stop", discordgo.DangerButton, se.StopNames)
	return out
}

// buttonRows packs buttons into action rows of five.
func buttonRows(buttons []discordgo.MessageComponent) []discordgo.MessageComponent {
	rows := noComponents()
	for start := 0; start < len(buttons); start += buttonsPerRow {
		rows = append(rows, discordgo.ActionsRow{Components: buttons[start:min(start+buttonsPerRow, len(buttons))]})
	}
	return rows
}

// subcommandChoices are the subcommands registered for every command.
func subcommandChoices(env string) []*discordgo.ApplicationCommandOption {
	return []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionSubCommand, Name: subcommandStart, Description: truncateRunes("Start or stop workloads in "+env, 100)},
		{Type: discordgo.ApplicationCommandOptionSubCommand, Name: subcommandStatus, Description: "Show running workloads, who started them and when they stop"},
		{Type: discordgo.ApplicationCommandOptionSubCommand, Name: subcommandSchedule, Description: "Reserve workloads to be ready at a time"},
	}
}
