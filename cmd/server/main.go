package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
		slog.Error("failed to load config", slog.String("error.message", err.Error()))
		return 1
	}

	// Set log level from LOG_LEVEL.
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
		slog.Error("failed to build DI container", slog.String("error.message", err.Error()))
		return 1
	}

	var (
		serverManager *server.Manager
		ms            *store.MultiStore
		registry      *config.Config
		httpSrv       *httpserver.Server
	)

	if err := c.Invoke(func(m *server.Manager, ks *store.MultiStore, r *config.Config, h *httpserver.Server) {
		serverManager = m
		ms = ks
		registry = r
		httpSrv = h
	}); err != nil {
		slog.Error("failed to resolve components", slog.String("error.message", err.Error()))
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bootstrap MultiStore once at startup (list ScaledObjects and restore state ConfigMap per cluster).
	// Runs all clusters in parallel with a 30s timeout.
	bootstrapCtx, bootstrapCancel := context.WithTimeout(ctx, 30*time.Second)
	defer bootstrapCancel()
	if err := ms.Bootstrap(bootstrapCtx); err != nil {
		slog.Error("MultiStore bootstrap failed", slog.String("error.message", err.Error()))
		return 1
	}
	httpSrv.MarkReady()

	// Startup log including the registry env list.
	envs := make([]string, 0)
	for _, cl := range registry.All() {
		envs = append(envs, cl.Env)
	}
	slog.Info("workload-knocker started", slog.Any("clusters", envs), slog.Int("cluster.count", len(envs)))

	// Hand HTTP server and Slack gateway to the Lifecycle Manager.
	// ctx.Done() shuts down all lifecycles in parallel; exit when Wait returns.
	if err := serverManager.Run(ctx); err != nil {
		slog.Error("lifecycle exited with error", slog.String("error.message", err.Error()))
		return 1
	}

	slog.Info("workload-knocker stopping")
	return 0
}
