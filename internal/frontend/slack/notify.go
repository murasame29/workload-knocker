package slackbot

import (
	"context"
	"errors"
	"log/slog"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend/notify"
)

// actionExtend is the action_id of the Extend button of a stop reminder.
const actionExtend = "extend_stop"

// chatAPI is the part of slack.Client the poster needs (scope chat:write).
type chatAPI interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
}

// poster implements notify.Poster with chat.postMessage and chat.update.
type poster struct{ api chatAPI }

var _ notify.Poster = poster{}

func (poster) Platform() string { return config.FrontendSlack }

// messageText is the section text (and notification fallback) of m, within Slack's section limit.
func messageText(m notify.Message) string {
	if r := []rune(m.Text); len(r) > slackSectionTextMaxLen {
		return string(r[:slackSectionTextMaxLen])
	}
	return m.Text
}

// messageBlocks renders m as a section plus an optional button row.
func messageBlocks(m notify.Message) []slack.Block {
	blocks := []slack.Block{slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", messageText(m), false, false), nil, nil)}
	if len(m.Buttons) > 0 {
		elems := make([]slack.BlockElement, 0, len(m.Buttons))
		for _, b := range m.Buttons {
			elems = append(elems, slack.NewButtonBlockElement(actionExtend, b.Payload, slack.NewTextBlockObject("plain_text", b.Label, false, false)))
		}
		blocks = append(blocks, slack.NewActionBlock("", elems...))
	}
	return blocks
}

func options(m notify.Message) []slack.MsgOption {
	return []slack.MsgOption{slack.MsgOptionText(messageText(m), false), slack.MsgOptionBlocks(messageBlocks(m)...)}
}

func (p poster) Post(ctx context.Context, channel string, m notify.Message) (string, error) {
	_, ts, err := p.api.PostMessageContext(ctx, channel, options(m)...)
	return ts, err
}

func (p poster) Edit(ctx context.Context, channel, ref string, m notify.Message) error {
	_, _, _, err := p.api.UpdateMessageContext(ctx, channel, ref, options(m)...)
	return err
}

// Poster is the notify.Poster of this bot, to be registered with the notify service.
func (b *Bot) Poster() notify.Poster { return poster{api: b.client} }

// handleExtend answers a press of the Extend button of a stop reminder.
func (b *Bot) handleExtend(callback slack.InteractionCallback, payload string) {
	reply := func(text string) {
		if _, err := b.client.PostEphemeral(callback.Channel.ID, callback.User.ID, slack.MsgOptionText(text, false)); err != nil {
			slog.Error("failed to post ephemeral message", slog.String("error.message", err.Error()))
		}
	}
	ref, err := notify.DecodeExtend(payload)
	if err != nil {
		slog.Warn("invalid extend button payload", slog.String("error.message", err.Error()))
		reply("⚠️ Unrecognized control.")
		return
	}
	stopAt, err := b.svc.Extender().Extend(context.Background(), notify.ExtendRequest{Subject: b.subject(callback.User.ID), Ref: ref, By: notify.ExtendBy})
	if err != nil && !errors.Is(err, notify.ErrExtendUnavailable) {
		slog.Info("extend refused", slog.String("cluster.env", ref.Env), slog.String("namespace", ref.Namespace), slog.String("workload", ref.Name), slog.String("error.message", err.Error()))
	}
	reply(notify.ExtendReply(ref, stopAt, err))
}
