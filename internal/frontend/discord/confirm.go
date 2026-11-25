package discordbot

import (
	"slices"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
)

// Discord buttons have no built-in confirmation, so the Stop button of an entry someone else
// started replaces the status list with a question; its buttons reuse the Stop button's payload.
const (
	kindStatusStopConfirmed = "sy" // Stop button of the confirmation: stop now (Token keys the button payload)
	kindStatusBack          = "sb" // Cancel button of the confirmation: back to the status list
)

// startedBy reports whether starter is userID. Discord starts and reservations both record the
// user ID. An unknown starter ("") is never someone else.
func startedBy(userID, starter string) bool { return starter == "" || starter == userID }

// foreignStarters returns the label and the starters of the running workloads of ref that someone
// other than userID started and sub may stop now, from the in-memory status.
func (b *Bot) foreignStarters(sub authz.Subject, userID string, ref frontend.StatusRef) (string, []string) {
	var label string
	var starters []string
	for _, ns := range b.svc.Status(sub, ref.Env).Namespaces {
		if ns.Name != ref.Namespace {
			continue
		}
		for _, se := range ns.Entries {
			for _, w := range se.Entry.Members {
				// Only stops the policy and mode allow now: a refused one goes straight to the refusal.
				if !slices.Contains(ref.Names, w.Name) || !slices.Contains(se.StopNames, w.Name) || startedBy(userID, w.StartedBy) {
					continue
				}
				label = se.Entry.Key
				if !slices.Contains(starters, w.StartedBy) {
					starters = append(starters, w.StartedBy)
				}
			}
		}
	}
	return label, starters
}

// stopConfirmView asks whether to stop a workload someone else started.
func stopConfirmView(cl config.Cluster, token, label string, starters []string) view {
	shown := make([]string, 0, len(starters))
	for _, s := range starters {
		shown = append(shown, displayStarter(s))
	}
	content := header(cl) + "\n" +
		"Stop: **" + label + "**  Started by: " + strings.Join(shown, ", ") + "\n" +
		"You did not start this workload.\n" +
		"Do you want to stop it?"
	return view{
		Content: truncateRunes(content, contentMaxLen),
		Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			button("Cancel", discordgo.SecondaryButton, customID{kindStatusBack, token, 0}, false),
			button("Stop", discordgo.DangerButton, customID{kindStatusStopConfirmed, token, 0}, false),
		}}},
	}
}

// foreignStopLines warns, in the change preview, about the stops of workloads someone else started.
func foreignStopLines(changes []frontend.Change, entries []frontend.Entry, userID string) []string {
	var lines []string
	for _, fs := range frontend.ForeignStops(changes, entries, func(s string) bool { return startedBy(userID, s) }) {
		shown := make([]string, 0, len(fs.Starters))
		for _, s := range fs.Starters {
			shown = append(shown, displayStarter(s))
		}
		lines = append(lines, "⚠ Stop: **"+fs.Label+"**  Started by: "+strings.Join(shown, ", "))
	}
	if len(lines) == 0 {
		return nil
	}
	return append(lines, "You did not start the workloads above. Confirm stops them anyway.")
}
