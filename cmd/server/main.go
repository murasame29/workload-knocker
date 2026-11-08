package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/newrelic/go-agent/v3/integrations/logcontext-v2/nrslog"
	"github.com/newrelic/go-agent/v3/newrelic"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/container"
	"github.com/murasame29/workload-knocker/internal/server"
	httpserver "github.com/murasame29/workload-knocker/internal/server/http"
	"github.com/murasame29/workload-knocker/internal/store"
	"github.com/murasame29/workload-knocker/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	logOpts := &slog.HandlerOptions{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, logOpts)))

	cfg, err := config.LoadEnv()
	if err != nil {
		slog.Error("設定の読み込みに失敗", slog.String("error.message", err.Error()))
		return 1
	}

	// LOG_LEVEL 環境変数からログレベルを設定
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		logOpts.Level = slog.LevelDebug
	case "warn":
		logOpts.Level = slog.LevelWarn
	case "error":
		logOpts.Level = slog.LevelError
	default:
		logOpts.Level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, logOpts)))

	slog.Info("starting workload-knocker",
		slog.String("log.level", cfg.LogLevel),
		slog.String("service.version", version.Version),
		slog.String("vcs.revision", version.Commit),
		slog.String("build.date", version.BuildDate),
	)

	c, err := container.BuildServer(cfg)
	if err != nil {
		slog.Error("DI コンテナの構築に失敗", slog.String("error.message", err.Error()))
		return 1
	}

	var (
		serverManager *server.Manager
		ms            *store.MultiStore
		registry      *config.Config
		nrApp         *newrelic.Application
		httpSrv       *httpserver.Server
	)

	if err := c.Invoke(func(m *server.Manager, ks *store.MultiStore, r *config.Config, nr *newrelic.Application, h *httpserver.Server) {
		serverManager = m
		ms = ks
		registry = r
		nrApp = nr
		httpSrv = h
	}); err != nil {
		slog.Error("コンポーネントの取得に失敗", slog.String("error.message", err.Error()))
		return 1
	}

	// New Relic の graceful shutdown
	if nrApp != nil {
		defer nrApp.Shutdown(5 * time.Second)
		// nrslog で slog を再設定し、NR-LINKING メタデータをログに付与する
		slog.SetDefault(slog.New(nrslog.WrapHandler(nrApp, slog.NewJSONHandler(os.Stdout, logOpts))))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// MultiStore を起動時に一度だけ初期化（全 cluster の ScaledObject list + state ConfigMap 復元）。
	// 30 秒のタイムアウトで全 cluster 並列実行する。
	bootstrapCtx, bootstrapCancel := context.WithTimeout(ctx, 30*time.Second)
	defer bootstrapCancel()
	if err := ms.Bootstrap(bootstrapCtx); err != nil {
		slog.Error("MultiStore の Bootstrap に失敗", slog.String("error.message", err.Error()))
		return 1
	}
	httpSrv.MarkReady()

	// 起動完了ログ: registry の env 一覧を含める
	envs := make([]string, 0)
	for _, cl := range registry.All() {
		envs = append(envs, cl.Env)
	}
	slog.Info("workload-knocker started", slog.Any("clusters", envs), slog.Int("cluster.count", len(envs)))

	// HTTP server / Slack gateway を Lifecycle Manager に任せる。
	// ctx.Done() で全 Lifecycle を並列に Shutdown し、Wait が戻ったら終了。
	if err := serverManager.Run(ctx); err != nil {
		slog.Error("ライフサイクルがエラーで終了", slog.String("error.message", err.Error()))
		return 1
	}

	slog.Info("workload-knocker を終了します")
	return 0
}
