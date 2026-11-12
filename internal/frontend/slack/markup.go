package slackbot

import "github.com/murasame29/workload-knocker/internal/frontend"

// markup formats the shared chat lines for Slack mrkdwn.
var markup = frontend.Markup{
	Bold:     func(s string) string { return "*" + s + "*" },
	Time:     slackTime,
	Relative: slackTime,
	Text:     escapeMrkdwn,
	Starter:  escapeMrkdwn,
}
