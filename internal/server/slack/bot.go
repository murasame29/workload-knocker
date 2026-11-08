// Package slackbot implements the Slack Gateway running on Slack Socket Mode as a frontend.Frontend.
package slackbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
)

const (
	// componentName is the return value of server.Lifecycle.Name.
	componentName = "slack"
	// maxConcurrentEvents caps concurrently processed Slack events.
	// Slash commands are human-driven, so 10 in parallel is plenty.
	maxConcurrentEvents = 10
)

// Bot is the workload control bot running on Slack Socket Mode.
type Bot struct {
	client   *slack.Client
	socket   *socketmode.Client
	svc      *frontend.Service
	registry config.Resolver
	authz    *authz.Holder
	groups   *groupCache
	// webhook posts to a response_url; replaced in tests.
	webhook func(ctx context.Context, url string, msg *slack.WebhookMessage) error

	// sem is a semaphore limiting concurrent goroutines.
	sem chan struct{}
	// inflight is a WaitGroup that lets Shutdown wait for in-flight handlers.
	inflight sync.WaitGroup
	// watchCtx ends rollout watches on shutdown; watchers lets Shutdown wait for them.
	watchCtx    context.Context
	watchCancel context.CancelFunc
	watchers    sync.WaitGroup
	// done lets Shutdown wait for the receive loop started by Start to stop.
	done chan struct{}
}

// New creates a Bot.
// registry resolves clusters from Slack slash commands on every request, so hot reload applies.
// az is the live authorization policy.
func New(cfg *config.Env, store model.WorkloadStore, registry config.Resolver, az *authz.Holder, m metrics.MetricsManager, opts ...frontend.Option) (*Bot, error) {
	if cfg.SlackAppToken == "" || cfg.SlackBotToken == "" {
		return nil, fmt.Errorf("slackbot.New: SLACK_APP_TOKEN and SLACK_BOT_TOKEN are required")
	}

	client := slack.New(
		cfg.SlackBotToken,
		slack.OptionAppLevelToken(cfg.SlackAppToken),
	)
	socket := socketmode.New(client)

	watchCtx, watchCancel := context.WithCancel(context.Background())
	return &Bot{
		watchCtx:    watchCtx,
		watchCancel: watchCancel,
		client:      client,
		socket:      socket,
		svc: frontend.NewService(store, m, cfg.ClusterReconcileTimeout,
			append([]frontend.Option{frontend.WithAuthorizer(az), frontend.WithMode(cfg.OperationMode)}, opts...)...),
		registry: registry,
		authz:    az,
		groups:   newGroupCache(client, groupCacheTTL),
		webhook:  slack.PostWebhookContext,
		sem:      make(chan struct{}, maxConcurrentEvents),
		done:     make(chan struct{}),
	}, nil
}

// Bot is a frontend.Frontend.
var _ frontend.Frontend = (*Bot)(nil)

// Name returns the stable identifier per the server.Lifecycle contract.
func (b *Bot) Name() string { return componentName }

// Start opens the Socket Mode connection. When ctx is canceled, socketmode.Client
// disconnects itself and returns, so everything stops together via the Manager signal path.
func (b *Bot) Start(ctx context.Context) error {
	defer close(b.done)
	go b.handleEvents()

	slog.Info("starting Slack Socket Mode connection")
	// socketmode.Client.RunContext currently always returns a non-nil error, so staticcheck
	// flags err != nil as always true; keep the check to avoid depending on library internals.
	//nolint:staticcheck // SA4023: defensive check in case RunContext returns nil in the future
	if err := b.socket.RunContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("slack socketmode run: %w", err)
	}
	return nil
}

// Shutdown waits for the receive loop to stop and in-flight handlers to finish.
// Disconnecting Socket Mode is handled by socketmode.Client when the ctx passed to Start is canceled.
func (b *Bot) Shutdown(ctx context.Context) error {
	b.watchCancel()
	// Wait for the receive loop to stop
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Wait for in-flight handlers within the ctx deadline
	doneCh := make(chan struct{})
	go func() {
		b.inflight.Wait()
		b.watchers.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Bot) handleEvents() {
	// Handle each event in a goroutine so handlers do not block receiving later events;
	// serial execution would hit the 3s Slack Ack timeout and make forms fail intermittently.
	// The semaphore caps concurrency at maxConcurrentEvents.
	for evt := range b.socket.Events {
		slog.Debug("socket event received", slog.String("event.type", string(evt.Type)), slog.Bool("has_request", evt.Request != nil))

		// Handle connection events (Request is nil) before spawning a goroutine.
		switch evt.Type {
		case socketmode.EventTypeConnecting:
			slog.Info("Slack socket mode connecting")
			continue
		case socketmode.EventTypeConnectionError:
			slog.Warn("Slack socket mode connection error", slog.Any("data", evt.Data))
			continue
		case socketmode.EventTypeConnected:
			slog.Info("Slack socket mode connected")
			continue
		case socketmode.EventTypeDisconnect:
			slog.Warn("Slack socket mode disconnected")
			continue
		case socketmode.EventTypeHello:
			slog.Debug("Slack socket mode hello")
			continue
		case socketmode.EventTypeIncomingError:
			slog.Warn("Slack socket mode incoming error", slog.Any("data", evt.Data))
			continue
		}

		b.sem <- struct{}{}
		b.inflight.Add(1)
		go func() {
			defer b.inflight.Done()
			defer func() { <-b.sem }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in slack event handler", slog.Any("recover", r), slog.String("stack", string(debug.Stack())))
				}
			}()

			switch evt.Type {
			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					slog.Warn("slash command type assertion failed")
					return
				}
				slog.Debug("slash command dispatched", slog.String("command", cmd.Command), slog.String("user.id", cmd.UserID), slog.String("trigger_id", cmd.TriggerID))
				b.ack(*evt.Request)
				b.handleSlashCommand(cmd)

			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					slog.Warn("interaction callback type assertion failed")
					return
				}
				slog.Debug("interactive event dispatched", slog.String("callback.type", string(callback.Type)), slog.String("callback.id", callback.CallbackID), slog.String("view.callback_id", callback.View.CallbackID), slog.String("user.id", callback.User.ID))
				if callback.Type == slack.InteractionTypeViewSubmission {
					// view_submission is acked inside each handler (a response_action payload is needed)
					b.handleViewSubmission(callback, evt.Request)
				} else {
					b.ack(*evt.Request)
					b.handleInteraction(callback)
				}

			default:
				slog.Debug("unhandled socket event", slog.String("event.type", string(evt.Type)))
			}
		}()
	}
}

// ack is a thin wrapper that logs b.socket.Ack errors at WARN.
// A failed Slack Ack is hard to recover from, so only log and keep running.
func (b *Bot) ack(req socketmode.Request, payload ...any) {
	if err := b.socket.Ack(req, payload...); err != nil {
		slog.Warn("socket mode ack failed", slog.String("error.message", err.Error()))
	}
}
