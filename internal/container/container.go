package container

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"go.uber.org/dig"

	"github.com/murasame29/workload-knocker/internal/api"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	k8sauth "github.com/murasame29/workload-knocker/internal/k8s/auth"
	"github.com/murasame29/workload-knocker/internal/k8s/auth/eks"
	"github.com/murasame29/workload-knocker/internal/k8s/auth/kubeconfig"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/reconcile"
	"github.com/murasame29/workload-knocker/internal/reload"
	"github.com/murasame29/workload-knocker/internal/server"
	discordbot "github.com/murasame29/workload-knocker/internal/server/discord"
	httpserver "github.com/murasame29/workload-knocker/internal/server/http"
	slackbot "github.com/murasame29/workload-knocker/internal/server/slack"
	"github.com/murasame29/workload-knocker/internal/store"
)

// BuildServer builds the DI container and registers all providers.
func BuildServer(cfg *config.Env) (*dig.Container, error) {
	c := dig.New()

	// Register externally supplied settings before the loop.
	if err := c.Provide(func() *config.Env { return cfg }); err != nil {
		return nil, fmt.Errorf("provide *config.Env: %w", err)
	}

	providers := []any{
		// MetricsManager
		func() metrics.MetricsManager { return metrics.New() },

		// AWS Config (IRSA + default credential chain), consumed by the eks Provider
		newAWSConfig,

		// EndpointProvider set: connection paths to managed clusters
		func(awsCfg aws.Config, m metrics.MetricsManager) (*eks.Provider, error) {
			return eks.New(awsCfg, m)
		},
		kubeconfig.New,

		// k8sauth.Multiplexer dispatches by provider type
		func(e *eks.Provider, kc *kubeconfig.Provider) *k8sauth.Multiplexer {
			return k8sauth.NewMultiplexer(e, kc)
		},

		// Cluster Registry: loads and validates the read-only mounted YAML at startup
		func(cfg *config.Env) (*config.Config, error) {
			reg, err := config.Load(cfg.ClusterConfigPath)
			if err != nil {
				return nil, err
			}
			if err := cfg.ValidateFrontends(reg); err != nil {
				return nil, err
			}
			return reg, nil
		},

		// Live registry (swapped on hot reload) and authorization policy (nil policy = disabled)
		config.NewHolder,
		newAuthzHolder,
		func(h *authz.Holder) authz.Authorizer { return h },

		// MultiStore, consuming k8sauth.Multiplexer as RestConfigSource
		newMultiStore,
		func(mux *k8sauth.Multiplexer, m metrics.MetricsManager) store.ClusterFactory {
			return store.NewClusterFactory(mux, m)
		},
		newWatchers,
		// WorkloadStore interface (for consumers)
		func(s *store.MultiStore) model.WorkloadStore { return s },

		// /api/v1/stop-all
		api.New,
		// /api/v1/start
		api.NewStartHandler,

		// HTTP Server (server.Lifecycle)
		func(cfg *config.Env, m metrics.MetricsManager, stopAll *api.StopAllHandler, start *api.StartHandler) *httpserver.Server {
			return httpserver.New(cfg.HTTPPort, m.Handler(),
				api.Authenticate(cfg.APIToken, api.RequireStop(cfg.OperationMode, stopAll)),
				api.Authenticate(cfg.APIToken, start))
		},
		// Chat frontends: only those configured by at least one cluster are enabled
		newFrontends,

		// Background reconcile (per-tick hooks for other features are passed here)
		func(cfg *config.Env, ws model.WorkloadStore, m metrics.MetricsManager) *reconcile.Loop {
			return reconcile.New(ws, m, cfg.ReconcileInterval, cfg.ClusterReconcileTimeout)
		},

		// Lifecycle Manager: single entry point to start and stop from main
		func(cfg *config.Env, h *httpserver.Server, fs []frontend.Frontend, ws reload.Watchers, rl *reconcile.Loop) *server.Manager {
			components := []server.Lifecycle{h, rl}
			for _, f := range fs {
				components = append(components, f)
			}
			for _, w := range ws {
				components = append(components, w)
			}
			return server.NewManager(cfg.ShutdownTimeout, components...)
		},
	}

	for _, p := range providers {
		if err := c.Provide(p); err != nil {
			return nil, fmt.Errorf("dig provide: %w", err)
		}
	}

	return c, nil
}

func newFrontends(cfg *config.Env, reg *config.Config, live *config.Holder, az *authz.Holder, ws model.WorkloadStore, m metrics.MetricsManager) ([]frontend.Frontend, error) {
	var fs []frontend.Frontend
	if reg.FrontendEnabled(config.FrontendSlack) {
		b, err := slackbot.New(cfg, ws, live, az, m)
		if err != nil {
			return nil, err
		}
		fs = append(fs, b)
	}
	if reg.FrontendEnabled(config.FrontendDiscord) {
		b, err := discordbot.New(cfg, ws, live, az, m)
		if err != nil {
			return nil, err
		}
		fs = append(fs, b)
	}
	return fs, nil
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

func newMultiStore(cfg *config.Env, reg *config.Config, mux *k8sauth.Multiplexer, m metrics.MetricsManager) (*store.MultiStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return store.NewMultiStore(ctx, reg, mux, m, cfg.StopAllTimeout)
}

// newAuthzHolder loads the policy; a missing default file disables authorization (allow all).
func newAuthzHolder(cfg *config.Env) (*authz.Holder, error) {
	p, path, err := authz.LoadConfigured(cfg.AuthzConfigPath)
	if err != nil {
		return nil, err
	}
	if p == nil {
		slog.Info("authorization disabled: every chat user and API caller may start and stop every workload",
			slog.String("reason", "AUTHZ_CONFIG_PATH is unset and the default policy file does not exist"), slog.String("path", path))
	} else {
		slog.Info("authorization policy loaded", slog.String("path", path), slog.Int("rules", p.Rules()), slog.Bool("default.allow", p.DefaultAllow()))
	}
	return authz.NewHolder(p), nil
}

// newWatchers wires the file watchers that hot reload the registry and the policy.
func newWatchers(cfg *config.Env, live *config.Holder, az *authz.Holder, ms *store.MultiStore, build store.ClusterFactory, m metrics.MetricsManager) reload.Watchers {
	rr := &reload.Registry{Path: cfg.ClusterConfigPath, Env: cfg, Config: live, Store: ms, Build: build, Metrics: m}
	rr.Seed()
	ar := &reload.Authz{Path: authz.PathOrDefault(cfg.AuthzConfigPath), Policy: az, Metrics: m}
	ar.Seed()
	return reload.Watchers{
		reload.NewWatcher("registry", rr.Path, reload.DefaultDebounce, rr.Reload),
		reload.NewWatcher("authz", ar.Path, reload.DefaultDebounce, ar.Reload),
	}
}
