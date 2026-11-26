package slackbot

import (
	"strings"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/frontend"
)

// Slack's caps of the confirmation dialog texts.
const (
	slackConfirmTitleMaxLen = 100
	slackConfirmTextMaxLen  = 300
)

// viewer is the Slack user a view is rendered for.
type viewer struct {
	ID   string
	Name string
}

func viewerOf(u slack.User) viewer { return viewer{ID: u.ID, Name: u.Name} }

// started reports whether starter is the viewer. Chat starts record the Slack user name and
// reservations the user ID, so both match. An unknown starter ("") is never someone else.
func (v viewer) started(starter string) bool {
	return starter == "" || starter == v.ID || (v.Name != "" && starter == v.Name)
}

// foreignStopConfirm is the dialog of a Stop button for an entry someone else started.
func (u ui) foreignStopConfirm(label, starter string) *slack.ConfirmationBlockObject {
	text := u.t("Stop: *%s*  Started by: %s\nYou did not start this workload.\nDo you want to stop it?", escapeMrkdwn(label), slackStarter(starter))
	return slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject("plain_text", truncateText(u.t("Stop a workload someone else started?"), slackConfirmTitleMaxLen), false, false),
		slack.NewTextBlockObject("mrkdwn", truncateText(text, slackConfirmTextMaxLen), false, false),
		slack.NewTextBlockObject("plain_text", u.t("Stop"), false, false),
		slack.NewTextBlockObject("plain_text", u.t("Cancel"), false, false),
	).WithStyle(slack.StyleDanger)
}

// foreignStopLines warns, in the change preview, about the stops of workloads someone else started.
// entries are the rendered entries the plan was built from.
func (u ui) foreignStopLines(changes []frontend.Change, entries []frontend.Entry, v viewer) []string {
	var lines []string
	for _, fs := range frontend.ForeignStops(changes, entries, v.started) {
		shown := make([]string, 0, len(fs.Starters))
		for _, s := range fs.Starters {
			shown = append(shown, slackStarter(s))
		}
		lines = append(lines, u.t("⚠️ Stop: *%s*  Started by: %s", escapeMrkdwn(fs.Label), strings.Join(shown, ", ")))
	}
	if len(lines) == 0 {
		return nil
	}
	return append(lines, u.t("You did not start the workloads above. Apply stops them anyway."))
}
