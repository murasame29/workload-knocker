// Package slackbot は Slack Socket Mode で動作する Slack Gateway を server.Lifecycle として実装する。
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
	// componentName は server.Lifecycle.Name の戻り値。
	componentName = "slack"
	// maxConcurrentEvents は同時に処理する Slack イベント数の上限。
	// Slack の slash command は人間操作なので 10 並列で十分に余裕がある。
	maxConcurrentEvents = 10
)

// Bot は Slack Socket Mode で動作するワークロード制御 Bot。
type Bot struct {
	client   *slack.Client
	socket   *socketmode.Client
	store    model.WorkloadStore
	registry *config.Config
	cfg      *config.Env
	tracer   nrx.Tracer

	// sem は同時実行 goroutine 数を制限するセマフォ。
	sem chan struct{}
	// inflight は Shutdown 時に in-flight ハンドラの完了を待つための WaitGroup。
	inflight sync.WaitGroup
	// done は Start で起動した受信ループの停止を Shutdown が待つために使う。
	done chan struct{}
}

// New は Bot を生成する。
// registry は Slack のスラッシュコマンドから cluster を解決するために使う。
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

// Name は server.Lifecycle 契約に基づく安定識別子を返す。
func (b *Bot) Name() string { return componentName }

// Start は Socket Mode 接続を開始する。ctx がキャンセルされると socketmode.Client が
// 自身で接続を切って戻るため、Manager 側のシグナル経路に従って一斉停止できる。
func (b *Bot) Start(ctx context.Context) error {
	defer close(b.done)
	go b.handleEvents()

	slog.Info("Slack Socket Mode 接続を開始します")
	// socketmode.Client.RunContext は再接続ループを回し続ける実装のため、現時点では
	// 必ず非 nil の error を返す。staticcheck はこれを検知して err != nil を
	// 常に真と指摘するが、ライブラリ側の実装詳細に依存したくないため nil 判定は残す。
	//nolint:staticcheck // SA4023: RunContext が将来 nil を返しても nil ラップしないよう防御的に判定する
	if err := b.socket.RunContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("slack socketmode run: %w", err)
	}
	return nil
}

// Shutdown は受信ループの停止と in-flight ハンドラの完了を待つ。
// Socket Mode の切断自体は Start に渡した ctx のキャンセルで socketmode.Client が処理する。
func (b *Bot) Shutdown(ctx context.Context) error {
	// 受信ループの停止を待つ
	select {
	case <-b.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	// in-flight ハンドラの完了を ctx 期限内に待つ
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
	// 各イベントを goroutine で処理し、ハンドラの実行が後続イベントの受信をブロックしないようにする。
	// シリアル実行だと Slack の 3 秒 Ack タイムアウトに抵触し、フォーム操作がたまに失敗する。
	// セマフォで同時実行数を maxConcurrentEvents に制限し、暴発を防ぐ。
	for evt := range b.socket.Events {
		slog.Debug("socket event received", slog.String("event.type", string(evt.Type)), slog.Bool("has_request", evt.Request != nil))

		// 接続系イベント (Request が nil) は goroutine 化前に処理する。
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
					// view_submission は各ハンドラ内で Ack する (response_action payload が必要なため)
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

// startTransaction は tracer に委譲する。tracer が no-op なら no-op span を返す。
func (b *Bot) startTransaction(name string) nrx.Span {
	return b.tracer.Start(name)
}

// ack は b.socket.Ack のエラーを WARN ログに集約する薄いラッパ。
// Slack 側の Ack 失敗はリカバリ困難なのでログだけ残してプロセスは継続する。
func (b *Bot) ack(req socketmode.Request, payload ...any) {
	if err := b.socket.Ack(req, payload...); err != nil {
		slog.Warn("socket mode ack failed", slog.String("error.message", err.Error()))
	}
}

// recordCustomEvent は tracer に委譲する。
func (b *Bot) recordCustomEvent(eventType string, params map[string]any) {
	b.tracer.RecordEvent(eventType, params)
}
