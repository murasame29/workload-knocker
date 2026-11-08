package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
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

	// ClusterReconcileTimeout is the Reconcile timeout when a Slack command is received.
	ClusterReconcileTimeout time.Duration `env:"CLUSTER_RECONCILE_TIMEOUT" envDefault:"2s"`

	// StopAllTimeout is the per-cluster timeout of /api/v1/stop-all.
	StopAllTimeout time.Duration `env:"STOP_ALL_TIMEOUT" envDefault:"10s"`

	// ShutdownTimeout is the timeout server.Manager gives each Lifecycle.Shutdown.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"5s"`

	SlackAppToken      string `env:"SLACK_APP_TOKEN,required"`
	SlackBotToken      string `env:"SLACK_BOT_TOKEN,required"`
	NewRelicLicenseKey string `env:"NEW_RELIC_LICENSE_KEY"  envDefault:""`
	NewRelicAppName    string `env:"NEW_RELIC_APP_NAME"     envDefault:"workload-knocker"`
	LogLevel           string `env:"LOG_LEVEL"              envDefault:"info"`
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

	return cfg, nil
}
