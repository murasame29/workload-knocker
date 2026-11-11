package container

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"go.uber.org/dig"

	"github.com/murasame29/workload-knocker/internal/actuator/kedaexternal"
	"github.com/murasame29/workload-knocker/internal/api"
	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/autostop"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	k8sauth "github.com/murasame29/workload-knocker/internal/k8s/auth"
	"github.com/murasame29/workload-knocker/internal/k8s/auth/eks"
	"github.com/murasame29/workload-knocker/internal/k8s/auth/kubeconfig"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/notify"
	"github.com/murasame29/workload-knocker/internal/reconcile"
	"github.com/murasame29/workload-knocker/internal/reload"
	"github.com/murasame29/workload-knocker/internal/reservation"
	"github.com/murasame29/workload-knocker/internal/rollout"
	"github.com/murasame29/workload-knocker/internal/runlimit"
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

		// Notify channel posts (start, ready, stop); chat adapters register their posters
		func(live *config.Holder) *notify.Service { return notify.New(live) },

		// /api/v1/stop-all
		func(ws model.WorkloadStore, m metrics.MetricsManager, az authz.Authorizer, n *notify.Service) *api.StopAllHandler {
			return api.New(ws, m, az, api.WithNotifier(n))
		},
		// Run duration settings, /api/v1/start and /api/v1/extend
		func(cfg *config.Env) runlimit.Defaults { return cfg.RunDefaults() },
		func(ws model.WorkloadStore, m metrics.MetricsManager, az authz.Authorizer, d runlimit.Defaults, n *notify.Service, rt *rollout.Tracker) *api.StartHandler {
			return api.NewStartHandler(ws, m, az, d, api.WithNotifier(n), api.WithTracker(rt))
		},
		func(ws model.WorkloadStore, az authz.Authorizer, d runlimit.Defaults) *api.ExtendHandler {
			return api.NewExtendHandler(ws, az, d)
		},
		// GET /api/v1/workloads
		func(ws model.WorkloadStore, az authz.Authorizer) *api.WorkloadsHandler {
			return api.NewWorkloadsHandler(ws, az)
		},

		// One-off reservations: executed by the background loop, created from chat and the API
		newReservations,
		func(r *reservation.Service) *api.ReservationsHandler { return api.NewReservationsHandler(r) },

		// HTTP Server (server.Lifecycle)
		func(cfg *config.Env, m metrics.MetricsManager, stopAll *api.StopAllHandler, start *api.StartHandler, extend *api.ExtendHandler, workloads *api.WorkloadsHandler, reservations *api.ReservationsHandler) *httpserver.Server {
			rec := audit.NewSlog()
			return httpserver.New(cfg.HTTPPort, m.Handler(),
				api.Authenticate(cfg.APIToken, rec, api.RequireStop(cfg.OperationMode, rec, stopAll)),
				api.Authenticate(cfg.APIToken, rec, start),
				api.Authenticate(cfg.APIToken, rec, extend),
				api.Authenticate(cfg.APIToken, rec, workloads),
				api.Authenticate(cfg.APIToken, rec, reservations))
		},
		// Rollout progress after Confirm: per-cluster readers and the tracker
		func(mux *k8sauth.Multiplexer, live *config.Holder) *rollout.Readers {
			return rollout.NewReaders(mux, live)
		},
		func(cfg *config.Env, r *rollout.Readers, n *notify.Service) *rollout.Tracker {
			return rollout.NewTracker(r, cfg.RolloutPollInterval, cfg.RolloutWatchTimeout,
				rollout.WithOnStarted(func(t rollout.Target) { n.Ready(t.Env, t.Namespace, t.Name) }))
		},
		// Chat frontends: only those configured by at least one cluster are enabled
		newFrontends,

		// Auto-stop of expired workloads; reminders go to the notify channel
		func(cfg *config.Env, ws model.WorkloadStore, m metrics.MetricsManager, n *notify.Service) *autostop.Hook {
			return autostop.New(ws, m, cfg.StopReminder, autostop.WithReminder(n))
		},

		// Background reconcile (per-tick hooks for other features are passed here)
		func(cfg *config.Env, ws model.WorkloadStore, m metrics.MetricsManager, as *autostop.Hook, rs *reservation.Service) *reconcile.Loop {
			return reconcile.New(ws, m, cfg.ReconcileInterval, cfg.ClusterReconcileTimeout, as, rs)
		},

		// KEDA external scaler gRPC server; nil unless a cluster uses keda-external
		newExternalScaler,

		// Lifecycle Manager: single entry point to start and stop from main
		func(cfg *config.Env, h *httpserver.Server, fs []frontend.Frontend, ws reload.Watchers, rl *reconcile.Loop, ext *kedaexternal.Server) *server.Manager {
			components := []server.Lifecycle{h, rl}
			if ext != nil {
				components = append(components, ext)
			}
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

// newReservations builds the reservation service; times in notify messages use the cluster's
// Discord time zone.
func newReservations(cfg *config.Env, ms *store.MultiStore, az authz.Authorizer, m metrics.MetricsManager, n *notify.Service, rt *rollout.Tracker, live *config.Holder) *reservation.Service {
	settings := reservation.Settings{
		Warmup: cfg.ReservationWarmup, Grace: cfg.ReservationGrace, Reminder: cfg.ReservationReminder, Run: cfg.RunDefaults(),
		Location: func(env string) *time.Location {
			if c, ok := live.ByEnv(env); ok {
				return c.Frontends.Discord.Location()
			}
			return nil
		},
	}
	return reservation.New(ms, az, settings, reservation.WithNotifier(n), reservation.WithWatcher(rt), reservation.WithMetrics(m))
}

func newFrontends(cfg *config.Env, reg *config.Config, live *config.Holder, az *authz.Holder, ws model.WorkloadStore, m metrics.MetricsManager, rt *rollout.Tracker, n *notify.Service, rs *reservation.Service) ([]frontend.Frontend, error) {
	var fs []frontend.Frontend
	opts := []frontend.Option{frontend.WithRollout(rt), frontend.WithNotifier(n), frontend.WithRunDefaults(cfg.RunDefaults()), frontend.WithClusters(live)}
	if reg.FrontendEnabled(config.FrontendSlack) {
		b, err := slackbot.New(cfg, ws, live, az, m, opts...)
		if err != nil {
			return nil, err
		}
		b.SetReservations(rs)
		n.AddPoster(b.Poster())
		fs = append(fs, b)
	}
	if reg.FrontendEnabled(config.FrontendDiscord) {
		b, err := discordbot.New(cfg, ws, live, az, m, opts...)
		if err != nil {
			return nil, err
		}
		b.SetReservations(rs)
		n.AddPoster(b.Poster())
		fs = append(fs, b)
	}
	return fs, nil
}

// newExternalScaler configures the shared hub and returns the gRPC server, or nil when no cluster
// uses keda-external at startup.
func newExternalScaler(cfg *config.Env, reg *config.Config) (*kedaexternal.Server, error) {
	hub := kedaexternal.Shared()
	hub.SetAddressMatch(cfg.ExternalScalerAddressMatch)
	for _, c := range reg.All() {
		if c.UsesActuator(config.ActuatorKEDAExternal) {
			return kedaexternal.NewServer(hub, cfg.ExternalScalerPort, cfg.ExternalScalerTLSCert, cfg.ExternalScalerTLSKey)
		}
	}
	return nil, nil
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
