// Package discordbot implements the Discord frontend (gateway websocket, slash command plus
// message components) as a frontend.Frontend.
package discordbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

const (
	componentName       = "discord"
	maxConcurrentEvents = 10
	// commandSyncInterval is how often the live registry is compared with the registered commands.
	commandSyncInterval = 15 * time.Second
)

// registry is the live (hot reloaded) view of the cluster registry used by the bot.
type registry interface {
	ByEnv(env string) (config.Cluster, bool)
	ByDiscordCommand(cmd string) (config.Cluster, bool)
	DiscordCommands() []string
}

// session is the subset of discordgo used by Bot, so tests can fake it without a network.
type session interface {
	Open() error
	Close() error
	AddHandler(handler any) func()
	// AppID is the application ID; valid after Open.
	AppID() string
	OverwriteCommands(guildID string, cmds []*discordgo.ApplicationCommand) error
	Respond(i *discordgo.Interaction, r *discordgo.InteractionResponse) error
	Edit(i *discordgo.Interaction, e *discordgo.WebhookEdit) error
	// SendMessage posts a channel message and returns its ID; EditMessage edits it.
	SendMessage(channelID string, m *discordgo.MessageSend) (string, error)
	EditMessage(e *discordgo.MessageEdit) error
}

// liveSession adapts *discordgo.Session to session.
type liveSession struct{ *discordgo.Session }

func (s liveSession) AppID() string {
	if s.State != nil && s.State.Application != nil && s.State.Application.ID != "" {
		return s.State.Application.ID
	}
	if s.State != nil && s.State.User != nil {
		return s.State.User.ID
	}
	return ""
}

func (s liveSession) OverwriteCommands(guildID string, cmds []*discordgo.ApplicationCommand) error {
	_, err := s.ApplicationCommandBulkOverwrite(s.AppID(), guildID, cmds)
	return err
}

func (s liveSession) Respond(i *discordgo.Interaction, r *discordgo.InteractionResponse) error {
	return s.InteractionRespond(i, r)
}

func (s liveSession) Edit(i *discordgo.Interaction, e *discordgo.WebhookEdit) error {
	_, err := s.InteractionResponseEdit(i, e)
	return err
}

func (s liveSession) SendMessage(channelID string, m *discordgo.MessageSend) (string, error) {
	msg, err := s.ChannelMessageSendComplex(channelID, m)
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

func (s liveSession) EditMessage(e *discordgo.MessageEdit) error {
	_, err := s.ChannelMessageEditComplex(e)
	return err
}

// Bot is the workload control bot running on the Discord gateway.
type Bot struct {
	sess     session
	svc      *frontend.Service
	registry registry
	poster   *poster
	guildID  string
	// registered is the command set last bulk-overwritten; only the Start goroutine touches it.
	registered []string
	flows      *flowStore
	// noModal forces the message-based flow; tests use it to exercise the fallback.
	noModal bool

	// watchCtx ends rollout watches on shutdown; watchers lets Shutdown wait for them.
	watchCtx    context.Context
	watchCancel context.CancelFunc
	watchers    sync.WaitGroup

	sem       chan struct{}
	inflight  sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// New creates a Bot. cfg.DiscordGuildID empty registers commands globally.
// registry is read on every request so hot reload applies; az is the live authorization policy.
func New(cfg *config.Env, store model.WorkloadStore, registry registry, az *authz.Holder, m metrics.MetricsManager, opts ...frontend.Option) (*Bot, error) {
	if cfg.DiscordBotToken == "" {
		return nil, errors.New("discordbot.New: DISCORD_BOT_TOKEN is required")
	}
	s, err := discordgo.New("Bot " + cfg.DiscordBotToken)
	if err != nil {
		return nil, fmt.Errorf("discordbot.New: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds
	svc := frontend.NewService(store, m, cfg.ClusterReconcileTimeout,
		append([]frontend.Option{frontend.WithAuthorizer(az), frontend.WithMode(cfg.OperationMode)}, opts...)...)
	return newBot(liveSession{s}, svc, registry, cfg.DiscordGuildID), nil
}

func newBot(sess session, svc *frontend.Service, registry registry, guildID string) *Bot {
	watchCtx, watchCancel := context.WithCancel(context.Background())
	return &Bot{
		sess:     sess,
		svc:      svc,
		registry: registry,
		poster:   newPoster(sess),
		guildID:  guildID,
		flows:    newFlowStore(flowTTL),
		sem:      make(chan struct{}, maxConcurrentEvents),
		done:     make(chan struct{}),

		watchCtx:    watchCtx,
		watchCancel: watchCancel,
	}
}

var _ frontend.Frontend = (*Bot)(nil)

// Name returns the stable identifier per the server.Lifecycle contract.
func (b *Bot) Name() string { return componentName }

// Start connects to the gateway, registers the configured commands and blocks until ctx is done.
func (b *Bot) Start(ctx context.Context) error {
	defer close(b.done)

	b.sess.AddHandler(func(_ *discordgo.Session, ic *discordgo.InteractionCreate) {
		b.dispatch(ic.Interaction)
	})
	b.sess.AddHandler(func(_ *discordgo.Session, e *discordgo.Event) {
		b.handleRawEvent(e)
	})

	slog.Info("starting Discord gateway connection")
	if err := b.sess.Open(); err != nil {
		return fmt.Errorf("discord gateway open: %w", err)
	}
	if err := b.registerCommands(); err != nil {
		return err
	}

	tick := time.NewTicker(commandSyncInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			b.syncCommands()
		}
	}
}

// syncCommands re-registers the application commands when a registry reload changed the command
// set. A failure is logged and retried on the next tick; the old commands keep working meanwhile.
func (b *Bot) syncCommands() {
	if slices.Equal(b.registry.DiscordCommands(), b.registered) {
		return
	}
	if err := b.registerCommands(); err != nil {
		slog.Error("discord command re-registration failed, will retry", slog.String("error.message", err.Error()))
	}
}

// registerCommands bulk-overwrites the application commands in the target scope with the
// commands configured in the registry (commands not listed there are removed).
func (b *Bot) registerCommands() error {
	names := b.registry.DiscordCommands()
	cmds := b.commands(names)
	scope := "global"
	if b.guildID != "" {
		scope = "guild " + b.guildID
	}
	if err := b.sess.OverwriteCommands(b.guildID, cmds); err != nil {
		return fmt.Errorf("register discord commands (%s): %w (check the bot was invited with the applications.commands scope)", scope, err)
	}
	b.registered = names
	slog.Info("discord commands registered", slog.String("scope", scope), slog.Any("commands", names))
	return nil
}

func (b *Bot) commands(names []string) []*discordgo.ApplicationCommand {
	cmds := make([]*discordgo.ApplicationCommand, 0, len(names))
	for _, name := range names {
		cl, ok := b.registry.ByDiscordCommand(name)
		if !ok {
			continue
		}
		cmds = append(cmds, &discordgo.ApplicationCommand{
			Name:        name,
			Type:        discordgo.ChatApplicationCommand,
			Description: truncateRunes("Start or stop workloads in "+cl.Env, 100),
			Options:     subcommandChoices(cl.Env),
		})
	}
	return cmds
}

// Shutdown waits for in-flight handlers, then closes the gateway connection (idempotent).
func (b *Bot) Shutdown(ctx context.Context) error {
	b.watchCancel()
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	waited := make(chan struct{})
	go func() {
		b.inflight.Wait()
		b.watchers.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.closeOnce.Do(func() { b.closeErr = b.sess.Close() })
	return b.closeErr
}

// dispatch routes one interaction; discordgo already calls handlers in their own goroutines.
func (b *Bot) dispatch(i *discordgo.Interaction) {
	b.guard(func() {
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			b.handleCommand(i)
		case discordgo.InteractionMessageComponent:
			b.handleComponent(i)
		case discordgo.InteractionModalSubmit:
			b.handleModal(i)
		default:
			slog.Debug("unhandled discord interaction type", slog.String("interaction.type", i.Type.String()))
		}
	})
}

// guard runs one handler under the concurrency limit, tracked for Shutdown and panic-safe.
func (b *Bot) guard(fn func()) {
	b.sem <- struct{}{}
	b.inflight.Add(1)
	defer b.inflight.Done()
	defer func() { <-b.sem }()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in discord interaction handler", slog.Any("recover", r), slog.String("stack", string(debug.Stack())))
		}
	}()
	fn()
}
