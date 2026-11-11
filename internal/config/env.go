package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"

	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// Env is the startup configuration of workload-knocker (from environment variables).
// Per-cluster settings live in the YAML file read by config.Load.
type Env struct {
	HTTPPort int `env:"HTTP_PORT" envDefault:"8080"`

	// ControllerNamespace is the namespace holding the state ConfigMap (workload-state).
	ControllerNamespace string `env:"CONTROLLER_NAMESPACE" envDefault:"workload-control"`

	// ClusterConfigPath is the absolute path of the Cluster Registry YAML.
	// In production the ConfigMap is mounted read-only into the Pod and this points to it.
	ClusterConfigPath string `env:"CLUSTER_CONFIG_PATH" envDefault:"/etc/workload-knocker/config.yaml"`

	// AuthzConfigPath is the authorization policy file. Empty means the default path, and
	// authorization is disabled when that file does not exist (see internal/authz).
	AuthzConfigPath string `env:"AUTHZ_CONFIG_PATH"`

	// ClusterReconcileTimeout is the Reconcile timeout when a chat command is received.
	ClusterReconcileTimeout time.Duration `env:"CLUSTER_RECONCILE_TIMEOUT" envDefault:"2s"`

	// ReconcileInterval is the period of the background ScaledObject reconcile; 0 disables it.
	ReconcileInterval time.Duration `env:"RECONCILE_INTERVAL" envDefault:"1m"`

	// StopAllTimeout is the per-cluster timeout of /api/v1/stop-all.
	StopAllTimeout time.Duration `env:"STOP_ALL_TIMEOUT" envDefault:"10s"`

	// ShutdownTimeout is the timeout server.Manager gives each Lifecycle.Shutdown.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"5s"`

	// RolloutPollInterval is how often a scale target is polled after Confirm.
	RolloutPollInterval time.Duration `env:"ROLLOUT_POLL_INTERVAL" envDefault:"5s"`

	// RolloutWatchTimeout is when rollout progress gives up. It must stay below
	// MaxRolloutWatchTimeout, the lifetime of a Discord interaction token.
	RolloutWatchTimeout time.Duration `env:"ROLLOUT_WATCH_TIMEOUT" envDefault:"10m"`

	// DefaultRunDuration is the auto-stop delay of a start without an explicit duration; 0 = none.
	// The workload-knocker.io/default-duration annotation overrides it per workload.
	DefaultRunDuration time.Duration `env:"DEFAULT_RUN_DURATION" envDefault:"0"`

	// MaxRunDuration caps the run duration of a start or extension; the
	// workload-knocker.io/max-duration annotation overrides it per workload.
	MaxRunDuration time.Duration `env:"MAX_RUN_DURATION" envDefault:"24h"`

	// StopReminder is how long before the auto-stop the reminder fires; 0 disables it.
	StopReminder time.Duration `env:"STOP_REMINDER" envDefault:"15m"`

	// ReservationWarmup is how long before a reservation's time its workloads start; the
	// workload-knocker.io/warmup annotation overrides it per workload.
	ReservationWarmup time.Duration `env:"RESERVATION_WARMUP" envDefault:"5m"`
	// ReservationGrace is how long after its start time a reservation still runs (controller was down).
	ReservationGrace time.Duration `env:"RESERVATION_GRACE" envDefault:"15m"`
	// ReservationReminder is how long before a reservation starts the reminder is posted; 0 disables it.
	ReservationReminder time.Duration `env:"RESERVATION_REMINDER" envDefault:"15m"`

	// OperationMode is full | scheduled-stop | off; see internal/mode.
	OperationMode mode.Mode `env:"OPERATION_MODE" envDefault:"full"`

	// APIToken is the bearer token for /api/v1/*; empty disables the API (fail closed).
	APIToken string `env:"API_TOKEN"`

	// SlackAppToken / SlackBotToken are required only when the Slack frontend is enabled.
	SlackAppToken string `env:"SLACK_APP_TOKEN"`
	SlackBotToken string `env:"SLACK_BOT_TOKEN"`

	// DiscordBotToken is required only when the Discord frontend is enabled.
	// DiscordGuildID scopes command registration to one guild (instant); empty registers globally.
	DiscordBotToken string `env:"DISCORD_BOT_TOKEN"`
	DiscordGuildID  string `env:"DISCORD_GUILD_ID"`

	// ExternalScalerPort is the KEDA external scaler gRPC port, served only while a cluster uses keda-external.
	// TLS is enabled when both cert and key paths are set; the port should stay cluster-internal.
	ExternalScalerPort         int    `env:"EXTERNAL_SCALER_PORT" envDefault:"9090"`
	ExternalScalerTLSCert      string `env:"EXTERNAL_SCALER_TLS_CERT"`
	ExternalScalerTLSKey       string `env:"EXTERNAL_SCALER_TLS_KEY"`
	ExternalScalerAddressMatch string `env:"EXTERNAL_SCALER_ADDRESS_MATCH" envDefault:"workload-knocker"`

	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`
}

// MaxRolloutWatchTimeout is the exclusive upper bound of ROLLOUT_WATCH_TIMEOUT.
const MaxRolloutWatchTimeout = 15 * time.Minute

// ValidateRollout checks the rollout progress settings.
func (e *Env) ValidateRollout() error {
	if e.RolloutPollInterval < time.Second {
		return fmt.Errorf("ROLLOUT_POLL_INTERVAL must be at least 1s, got %s", e.RolloutPollInterval)
	}
	if e.RolloutWatchTimeout <= e.RolloutPollInterval || e.RolloutWatchTimeout >= MaxRolloutWatchTimeout {
		return fmt.Errorf("ROLLOUT_WATCH_TIMEOUT must be greater than ROLLOUT_POLL_INTERVAL and below %s (Discord interaction token lifetime), got %s", MaxRolloutWatchTimeout, e.RolloutWatchTimeout)
	}
	return nil
}

// ValidateRunDuration checks the run duration settings.
func (e *Env) ValidateRunDuration() error {
	if e.MaxRunDuration < runlimit.MinDuration {
		return fmt.Errorf("MAX_RUN_DURATION must be at least %s, got %s", runlimit.MinDuration, e.MaxRunDuration)
	}
	if e.DefaultRunDuration < 0 || (e.DefaultRunDuration > 0 && e.DefaultRunDuration < runlimit.MinDuration) {
		return fmt.Errorf("DEFAULT_RUN_DURATION must be 0 or at least %s, got %s", runlimit.MinDuration, e.DefaultRunDuration)
	}
	if e.DefaultRunDuration > e.MaxRunDuration {
		return fmt.Errorf("DEFAULT_RUN_DURATION %s exceeds MAX_RUN_DURATION %s", e.DefaultRunDuration, e.MaxRunDuration)
	}
	if e.StopReminder < 0 {
		return fmt.Errorf("STOP_REMINDER must not be negative, got %s", e.StopReminder)
	}
	return nil
}

// ValidateReservations checks the reservation settings.
func (e *Env) ValidateReservations() error {
	for name, d := range map[string]time.Duration{"RESERVATION_WARMUP": e.ReservationWarmup, "RESERVATION_GRACE": e.ReservationGrace, "RESERVATION_REMINDER": e.ReservationReminder} {
		if d < 0 {
			return fmt.Errorf("%s must not be negative, got %s", name, d)
		}
	}
	return nil
}

// RunDefaults returns the controller-wide run duration settings.
func (e *Env) RunDefaults() runlimit.Defaults {
	return runlimit.Defaults{Default: e.DefaultRunDuration, Max: e.MaxRunDuration}
}

// ValidateFrontends checks the env required by the frontends enabled in the registry.
func (e *Env) ValidateFrontends(reg *Config) error {
	if reg.FrontendEnabled(FrontendSlack) && (e.SlackAppToken == "" || e.SlackBotToken == "") {
		return errors.New("SLACK_APP_TOKEN and SLACK_BOT_TOKEN are required when a cluster configures frontends.slack")
	}
	if reg.FrontendEnabled(FrontendDiscord) && e.DiscordBotToken == "" {
		return errors.New("DISCORD_BOT_TOKEN is required when a cluster configures frontends.discord")
	}
	return nil
}

// LoadEnv builds Env from environment variables (reading a .env file if present).
// It logs a deprecation warning when WORKLOAD_ENV is set.
func LoadEnv() (*Env, error) {
	_ = godotenv.Load()

	cfg := &Env{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("config.LoadEnv: %w", err)
	}

	if err := cfg.ValidateRollout(); err != nil {
		return nil, fmt.Errorf("config.LoadEnv: %w", err)
	}
	if err := cfg.ValidateRunDuration(); err != nil {
		return nil, fmt.Errorf("config.LoadEnv: %w", err)
	}
	if err := cfg.ValidateReservations(); err != nil {
		return nil, fmt.Errorf("config.LoadEnv: %w", err)
	}

	if v := os.Getenv("WORKLOAD_ENV"); v != "" {
		slog.Warn("WORKLOAD_ENV is deprecated. Configure cluster env in cluster registry config file.", slog.String("workload_env", v))
	}

	if cfg.ReconcileInterval < 0 {
		return nil, fmt.Errorf("config.LoadEnv: RECONCILE_INTERVAL must not be negative, got %s", cfg.ReconcileInterval)
	}

	if cfg.ReconcileInterval == 0 && (cfg.DefaultRunDuration > 0 || cfg.StopReminder > 0) {
		slog.Warn("RECONCILE_INTERVAL is 0: run durations, reminders and reservations are stored but never fire, because the background loop drives them")
	}

	if cfg.APIToken == "" {
		slog.Warn("API_TOKEN is not set: /api/v1/* responds 503 until it is configured")
	}

	return cfg, nil
}

// DefaultGitOpsManagers are the field managers whose ownership of spec.replicas the direct actuator respects.
const DefaultGitOpsManagers = "argocd-controller,kustomize-controller,helm-controller"

// GitOpsManagers returns the comma-separated GITOPS_MANAGERS list, or the default when it is empty.
func GitOpsManagers() []string {
	raw := os.Getenv("GITOPS_MANAGERS")
	if strings.TrimSpace(raw) == "" {
		raw = DefaultGitOpsManagers
	}
	var out []string
	for _, m := range strings.Split(raw, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}
