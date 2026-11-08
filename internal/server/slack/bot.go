// Package slackbot implements the Slack Gateway running on Slack Socket Mode as a server.Lifecycle.
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

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/model"
	nrx "github.com/murasame29/workload-knocker/pkg/newrelic"
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
	store    model.WorkloadStore
	registry *config.Config
	cfg      *config.Env
	tracer   nrx.Tracer

	// sem is a semaphore limiting concurrent goroutines.
	sem chan struct{}
	// inflight is a WaitGroup that lets Shutdown wait for in-flight handlers.
	inflight sync.WaitGroup
	// done lets Shutdown wait for the receive loop started by Start to stop.
	done chan struct{}
}

// New creates a Bot.
// registry resolves clusters from Slack slash commands.
func New(cfg *config.Env, store model.WorkloadStore, registry *config.Config, tracer nrx.Tracer) (*Bot, error) {
	if cfg.SlackAppToken == "" || cfg.SlackBotToken == "" {
		return nil, fmt.Errorf("slackbot.New: slack tokens are required")
	}

	client := slack.New(
		cfg.SlackBotToken,
		slack.OptionAppLevelToken(cfg.SlackAppToken),
	)
	socket := socketmode.New(client)

	return &Bot{
		client:   client,
		socket:   socket,
		store:    store,
		registry: registry,
		cfg:      cfg,
		tracer:   tracer,
		sem:      make(chan struct{}, maxConcurrentEvents),
		done:     make(chan struct{}),
	}, nil
}

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

// startTransaction delegates to the tracer; a no-op tracer yields a no-op span.
func (b *Bot) startTransaction(name string) nrx.Span {
	return b.tracer.Start(name)
}

// ack is a thin wrapper that logs b.socket.Ack errors at WARN.
// A failed Slack Ack is hard to recover from, so only log and keep running.
func (b *Bot) ack(req socketmode.Request, payload ...any) {
	if err := b.socket.Ack(req, payload...); err != nil {
		slog.Warn("socket mode ack failed", slog.String("error.message", err.Error()))
	}
}

// recordCustomEvent delegates to the tracer.
func (b *Bot) recordCustomEvent(eventType string, params map[string]any) {
	b.tracer.RecordEvent(eventType, params)
}
