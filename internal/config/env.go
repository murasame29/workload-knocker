package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"

	"github.com/murasame29/workload-knocker/internal/mode"
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

	// StopAllTimeout is the per-cluster timeout of /api/v1/stop-all.
	StopAllTimeout time.Duration `env:"STOP_ALL_TIMEOUT" envDefault:"10s"`

	// ShutdownTimeout is the timeout server.Manager gives each Lifecycle.Shutdown.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"5s"`

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

	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`
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

	if v := os.Getenv("WORKLOAD_ENV"); v != "" {
		slog.Warn("WORKLOAD_ENV is deprecated. Configure cluster env in cluster registry config file.", slog.String("workload_env", v))
	}

	if cfg.APIToken == "" {
		slog.Warn("API_TOKEN is not set: /api/v1/* responds 503 until it is configured")
	}

	return cfg, nil
}
