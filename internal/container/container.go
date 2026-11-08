package container

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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
			return config.Load(cfg.ClusterConfigPath)
		},

		// MultiStore, consuming k8sauth.Multiplexer as RestConfigSource
		newMultiStore,
		// WorkloadStore interface (for consumers)
		func(s *store.MultiStore) model.WorkloadStore { return s },

		// /api/v1/stop-all
		api.New,
		// /api/v1/start
		api.NewStartHandler,

		// HTTP Server (server.Lifecycle)
		func(cfg *config.Env, m metrics.MetricsManager, stopAll *api.StopAllHandler, start *api.StartHandler) *httpserver.Server {
			return httpserver.New(cfg.HTTPPort, m.Handler(), stopAll, start)
		},
		// Slack Bot (server.Lifecycle)
		slackbot.New,

		// Lifecycle Manager: single entry point to start and stop from main
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
