package slackbot

import (
	"regexp"

	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/frontend/i18n"
)

// ui renders the Slack views and messages in one language (UI_LANGUAGE).
type ui struct {
	lang i18n.Lang
	// markup formats the shared chat lines for Slack mrkdwn.
	markup frontend.Markup
}

func newUI(lang i18n.Lang) ui {
	return ui{lang: lang, markup: frontend.Markup{
		Bold:     func(s string) string { return "*" + s + "*" },
		Time:     slackTime,
		Relative: slackTime,
		Text:     escapeMrkdwn,
		Starter:  slackStarter,
		Lang:     lang,
	}}
}

// t translates a message; the English format string is the key (see internal/frontend/i18n).
func (u ui) t(format string, args ...any) string { return u.lang.T(format, args...) }

// slackUserID matches a Slack user ID such as U012AB3CD or W012AB3CD.
var slackUserID = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)

// slackStarter renders who started a workload. Reservations record the creator's Slack user ID,
// which is shown as a mention so Slack displays the user's name; other values (the user name of a
// chat start, the user of an API call) are shown as text.
func slackStarter(s string) string {
	if slackUserID.MatchString(s) {
		return "<@" + s + ">"
	}
	return escapeMrkdwn(s)
}
