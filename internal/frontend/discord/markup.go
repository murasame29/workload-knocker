package discordbot

import "github.com/murasame29/workload-knocker/internal/frontend"

// markup formats the shared chat lines for Discord markdown.
var markup = frontend.Markup{
	Bold:     func(s string) string { return "**" + s + "**" },
	Time:     stopTime,
	Relative: relativeTime,
	Text:     func(s string) string { return s },
	Starter:  displayStarter,
}
