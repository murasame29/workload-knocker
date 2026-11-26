package discordbot

import (
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/frontend/i18n"
)

// ui renders the Discord views and messages in one language (UI_LANGUAGE).
type ui struct {
	lang i18n.Lang
	// markup formats the shared chat lines for Discord markdown.
	markup frontend.Markup
}

func newUI(lang i18n.Lang) ui {
	return ui{lang: lang, markup: frontend.Markup{
		Bold:     func(s string) string { return "**" + s + "**" },
		Time:     stopTime,
		Relative: relativeTime,
		Text:     func(s string) string { return s },
		Starter:  displayStarter,
		Lang:     lang,
	}}
}

// t translates a message; the English format string is the key (see internal/frontend/i18n).
func (u ui) t(format string, args ...any) string { return u.lang.T(format, args...) }
