package discordbot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/notify"
)

const (
	// buttonTTL is how long a posted button stays usable; custom_id is limited to 100 characters,
	// so the payload lives here and the custom_id carries a token.
	buttonTTL  = 24 * time.Hour
	maxButtons = 1000
)

// poster implements notify.Poster on the Discord session.
type poster struct {
	sess    session
	buttons *buttonStore
}

var _ notify.Poster = (*poster)(nil)

func newPoster(sess session) *poster {
	return &poster{sess: sess, buttons: &buttonStore{entries: map[string]buttonEntry{}, now: time.Now}}
}

func (p *poster) Platform() string { return config.FrontendDiscord }

func (p *poster) components(buttons []notify.Button) ([]discordgo.MessageComponent, error) {
	if len(buttons) == 0 {
		return noComponents(), nil
	}
	row := discordgo.ActionsRow{}
	for _, b := range buttons {
		token, err := p.buttons.put(b.Payload)
		if err != nil {
			return nil, err
		}
		row.Components = append(row.Components, button(b.Label, discordgo.PrimaryButton, customID{kindExtend, token, 0}, false))
	}
	return []discordgo.MessageComponent{row}, nil
}

func (p *poster) Post(_ context.Context, channel string, m notify.Message) (string, error) {
	comps, err := p.components(m.Buttons)
	if err != nil {
		return "", err
	}
	return p.sess.SendMessage(channel, &discordgo.MessageSend{Content: truncateRunes(m.Text, contentMaxLen), Components: comps, AllowedMentions: noMentions()})
}

func (p *poster) Edit(_ context.Context, channel, ref string, m notify.Message) error {
	comps, err := p.components(m.Buttons)
	if err != nil {
		return err
	}
	content := truncateRunes(m.Text, contentMaxLen)
	edit := discordgo.NewMessageEdit(channel, ref).SetContent(content)
	edit.Components, edit.AllowedMentions = &comps, noMentions()
	return p.sess.EditMessage(edit)
}

// Poster is the notify.Poster of this bot, to be registered with the notify service.
func (b *Bot) Poster() notify.Poster { return b.poster }

// handleExtend answers a press of the Extend button of a stop reminder.
func (b *Bot) handleExtend(i *discordgo.Interaction, id customID) {
	payload, ok := b.poster.buttons.get(id.Token)
	if !ok {
		b.replyEphemeral(i, msgExpired)
		return
	}
	ref, err := notify.DecodeExtend(payload)
	if err != nil {
		slog.Warn("invalid extend button payload", slog.String("error.message", err.Error()))
		b.replyEphemeral(i, msgUnrecognized)
		return
	}
	stopAt, err := b.svc.Extender().Extend(context.Background(), notify.ExtendRequest{Subject: subject(i), Ref: ref, By: notify.ExtendBy})
	if err != nil && !errors.Is(err, notify.ErrExtendUnavailable) {
		slog.Info("extend refused", slog.String("cluster.env", ref.Env), slog.String("namespace", ref.Namespace), slog.String("workload", ref.Name), slog.String("error.message", err.Error()))
	}
	b.replyEphemeral(i, notify.ExtendReply(ref, stopAt, err))
}

type buttonEntry struct {
	payload string
	expires time.Time
}

// buttonStore maps the token in a button custom_id to its payload.
type buttonStore struct {
	mu      sync.Mutex
	entries map[string]buttonEntry
	now     func() time.Time
}

func (s *buttonStore) put(payload string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("button token: %w", err)
	}
	token := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.entries {
		if !e.expires.After(now) {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= maxButtons {
		var oldest string
		var at time.Time
		for k, e := range s.entries {
			if oldest == "" || e.expires.Before(at) {
				oldest, at = k, e.expires
			}
		}
		delete(s.entries, oldest)
	}
	s.entries[token] = buttonEntry{payload: payload, expires: now.Add(buttonTTL)}
	return token, nil
}

func (s *buttonStore) get(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[token]
	if !ok || !e.expires.After(s.now()) {
		delete(s.entries, token)
		return "", false
	}
	return e.payload, true
}
