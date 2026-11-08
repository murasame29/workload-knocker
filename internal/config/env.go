package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Env は workload-knocker の起動設定 (環境変数由来)。
// cluster ごとの設定は config.Load で読み込む YAML ファイル側にある。
type Env struct {
	HTTPPort int `env:"HTTP_PORT" envDefault:"8080"`

	// ControllerNamespace は state ConfigMap (workload-state) の置き場所となる namespace。
	ControllerNamespace string `env:"CONTROLLER_NAMESPACE" envDefault:"workload-control"`

	// ClusterConfigPath は Cluster Registry YAML の絶対パス。
	// 本番では ConfigMap を Pod に RO で volume mount し、その path を指す想定。
	ClusterConfigPath string `env:"CLUSTER_CONFIG_PATH" envDefault:"/etc/workload-knocker/config.yaml"`

	// ClusterReconcileTimeout は Slack コマンド受信時の Reconcile タイムアウト。
	ClusterReconcileTimeout time.Duration `env:"CLUSTER_RECONCILE_TIMEOUT" envDefault:"2s"`

	// StopAllTimeout は /api/v1/stop-all の per-cluster タイムアウト。
	StopAllTimeout time.Duration `env:"STOP_ALL_TIMEOUT" envDefault:"10s"`

	// ShutdownTimeout は server.Manager が各 Lifecycle.Shutdown に与えるタイムアウト。
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"5s"`

	SlackAppToken      string `env:"SLACK_APP_TOKEN,required"`
	SlackBotToken      string `env:"SLACK_BOT_TOKEN,required"`
	NewRelicLicenseKey string `env:"NEW_RELIC_LICENSE_KEY"  envDefault:""`
	NewRelicAppName    string `env:"NEW_RELIC_APP_NAME"     envDefault:"workload-knocker"`
	LogLevel           string `env:"LOG_LEVEL"              envDefault:"info"`
}

// LoadEnv は環境変数 (.env ファイルがあれば読込) から Env を構築する。
// WORKLOAD_ENV が設定されている場合は deprecation warning を出す。
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
