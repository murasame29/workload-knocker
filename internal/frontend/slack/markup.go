package slackbot

import (
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
		Starter:  escapeMrkdwn,
		Lang:     lang,
	}}
}

// t translates a message; the English format string is the key (see internal/frontend/i18n).
func (u ui) t(format string, args ...any) string { return u.lang.T(format, args...) }
