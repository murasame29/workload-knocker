package container

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/newrelic/go-agent/v3/newrelic"
	"go.uber.org/dig"

	"github.com/murasame29/workload-knocker/internal/api"
	"github.com/murasame29/workload-knocker/internal/config"
	k8sauth "github.com/murasame29/workload-knocker/internal/k8s/auth"
	"github.com/murasame29/workload-knocker/internal/k8s/auth/eks"
	"github.com/murasame29/workload-knocker/internal/k8s/auth/kubeconfig"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/server"
	httpserver "github.com/murasame29/workload-knocker/internal/server/http"
	slackbot "github.com/murasame29/workload-knocker/internal/server/slack"
	"github.com/murasame29/workload-knocker/internal/store"
	nrx "github.com/murasame29/workload-knocker/pkg/newrelic"
)

// BuildServer は DI コンテナを構築する。
// providers の各 constructor を順に dig.Container へ登録する。
func BuildServer(cfg *config.Env) (*dig.Container, error) {
	c := dig.New()

	// 起動時に外部から渡される設定値はループの外で先に登録する。
	if err := c.Provide(func() *config.Env { return cfg }); err != nil {
		return nil, fmt.Errorf("provide *config.Env: %w", err)
	}

	providers := []any{
		// MetricsManager
		func() metrics.MetricsManager { return metrics.New() },

		// New Relic Application — license key 未設定なら nil (no-op)
		newNewRelicApp,

		// Tracer — *newrelic.Application を nil-safe に wrap
		nrx.New,

		// AWS Config (IRSA + default credential chain) — eks Provider が消費
		newAWSConfig,

		// EndpointProvider 群: managed cluster への接続経路
		func(awsCfg aws.Config, m metrics.MetricsManager) (*eks.Provider, error) {
			return eks.New(awsCfg, m)
		},
		kubeconfig.New,

		// k8sauth.Multiplexer は provider type で dispatch
		func(e *eks.Provider, kc *kubeconfig.Provider) *k8sauth.Multiplexer {
			return k8sauth.NewMultiplexer(e, kc)
		},

		// Cluster Registry — RO mount された YAML を起動時に読み込み + 検証
		func(cfg *config.Env) (*config.Config, error) {
			return config.Load(cfg.ClusterConfigPath)
		},

		// MultiStore — k8sauth.Multiplexer を RestConfigSource として消費
		newMultiStore,
		// WorkloadStore interface (consumer 側はこちら)
		func(s *store.MultiStore) model.WorkloadStore { return s },

		// /api/v1/stop-all
		api.New,
		// /api/v1/start
		api.NewStartHandler,

		// HTTP Server (server.Lifecycle)
		func(cfg *config.Env, m metrics.MetricsManager, stopAll *api.StopAllHandler, start *api.StartHandler, nrApp *newrelic.Application) *httpserver.Server {
			return httpserver.New(cfg.HTTPPort, m.Handler(), stopAll, start, nrApp)
		},
		// Slack Bot (server.Lifecycle)
		slackbot.New,

		// Lifecycle Manager — main から1点で起動・停止する
		func(cfg *config.Env, h *httpserver.Server, b *slackbot.Bot) *server.Manager {
			return server.NewManager(cfg.ShutdownTimeout, h, b)
		},
	}

	for _, p := range providers {
		if err := c.Provide(p); err != nil {
			return nil, fmt.Errorf("dig provide: %w", err)
		}
	}

	return c, nil
}

// newNewRelicApp は New Relic Application のコンストラクタ。
// license key 未設定時 / 初期化失敗時は nil を返す (Tracer 側で no-op になる)。
func newNewRelicApp(cfg *config.Env) *newrelic.Application {
	if cfg.NewRelicLicenseKey == "" {
		slog.Warn("NEW_RELIC_LICENSE_KEY が未設定のため New Relic は無効です")
		return nil
	}
	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName(cfg.NewRelicAppName),
		newrelic.ConfigLicense(cfg.NewRelicLicenseKey),
		newrelic.ConfigDistributedTracerEnabled(true),
		newrelic.ConfigAppLogEnabled(false),
		newrelic.ConfigAppLogForwardingEnabled(false),
		newrelic.ConfigAppLogDecoratingEnabled(true),
	)
	if err != nil {
		slog.Warn("New Relic の初期化に失敗", slog.String("error.message", err.Error()))
		return nil
	}
	slog.Info("New Relic APM を初期化しました", slog.String("app_name", cfg.NewRelicAppName))
	return app
}

func newAWSConfig() (aws.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return aws.Config{}, fmt.Errorf("aws config load: %w", err)
	}
	return awsCfg, nil
}

func newMultiStore(cfg *config.Env, reg *config.Config, mux *k8sauth.Multiplexer, m metrics.MetricsManager, tracer nrx.Tracer) (*store.MultiStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return store.NewMultiStore(ctx, reg, mux, m, tracer, cfg.StopAllTimeout)
}
