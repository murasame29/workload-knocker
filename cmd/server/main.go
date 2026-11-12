package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/murasame29/workload-knocker/internal/app/container"
	"github.com/murasame29/workload-knocker/internal/app/server"
	"github.com/murasame29/workload-knocker/internal/config"
	httpserver "github.com/murasame29/workload-knocker/internal/frontend/http"
	"github.com/murasame29/workload-knocker/internal/observability/logging"
	"github.com/murasame29/workload-knocker/internal/observability/version"
	"github.com/murasame29/workload-knocker/internal/workload/store"
)

func main() {
	os.Exit(run())
}

func run() int {
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get())
		return 0
	}

	vi := version.Get()
	level := new(slog.LevelVar)
	slog.SetDefault(logging.New(os.Stdout, level, vi.Version))

	cfg, err := config.LoadEnv()
	if err != nil {
		slog.Error("failed to load config", slog.String("error.message", err.Error()))
		return 1
	}

	// Set log level from LOG_LEVEL.
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}

	slog.Info("starting workload-knocker",
		slog.String("log.level", cfg.LogLevel),
		slog.String("vcs.revision", vi.Commit),
		slog.String("build.date", vi.BuildDate),
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
