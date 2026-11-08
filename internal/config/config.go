// Package config は workload-knocker の設定を扱う。
//
//   - 環境変数経由のプロセス設定 (env.go: Env, LoadEnv)
//   - Cluster Registry の YAML スキーマと lookup (config.go: Config, Load)
//
// Cluster Registry はファイル経由で読み込む (本番では ConfigMap を RO で volume mount する)。
// このため config パッケージは K8s API に依存しない。
package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Provider type 識別子。
const (
	ProviderTypeEKS        = "eks"
	ProviderTypeKubeconfig = "kubeconfig"
)

// Config は Cluster Registry の root スキーマ + 検証済み lookup を兼ねる。
// Validate / Load を通った後は immutable として扱う (hot-reload 対象外)。
type Config struct {
	Controller Controller `yaml:"controller"`
	Clusters   []Cluster  `yaml:"clusters"`

	byEnv          map[string]Cluster
	bySlashCommand map[string]Cluster
}

// Controller は workload-knocker Pod が動作するクラスタの設定。
type Controller struct {
	// Namespace は state ConfigMap (workload-state) の置き場所。
	Namespace string `yaml:"namespace"`
}

// Cluster は managed クラスタ 1 つの設定。
type Cluster struct {
	// Env は env 識別子 (例: stg, stg-us, dev)。registry 内で一意。metrics ラベル / Modal の key にも使う。
	Env string `yaml:"env"`

	// Name は cluster name。metrics の cluster ラベル / EKS DescribeCluster の引数に使う。
	Name string `yaml:"name"`

	Provider ClusterProvider `yaml:"provider"`
	Slack    ClusterSlack    `yaml:"slack"`
}

// ClusterProvider は cluster への接続方法を表す discriminated union。
// Type と一致するフィールドのみ参照される。
type ClusterProvider struct {
	Type       string              `yaml:"type"`
	EKS        *ProviderEKS        `yaml:"eks,omitempty"`
	Kubeconfig *ProviderKubeconfig `yaml:"kubeconfig,omitempty"`
}

// ProviderEKS は AWS EKS への接続設定 (IRSA + 任意の cross-account AssumeRole)。
type ProviderEKS struct {
	Region        string `yaml:"region"`
	AssumeRoleArn string `yaml:"assumeRoleArn,omitempty"`
}

// ProviderKubeconfig は kubeconfig ファイル経由の接続設定。
type ProviderKubeconfig struct {
	Path    string `yaml:"path"`
	Context string `yaml:"context,omitempty"`
}

// ClusterSlack は Slack 関連設定。SlashCommand は registry 内で一意。
type ClusterSlack struct {
	SlashCommand string `yaml:"slashCommand"`
}

// All は登録された全 cluster のコピーを返す。
func (c *Config) All() []Cluster {
	out := make([]Cluster, len(c.Clusters))
	copy(out, c.Clusters)
	return out
}

func (c *Config) ByEnv(env string) (Cluster, bool) {
	cl, ok := c.byEnv[env]
	return cl, ok
}

func (c *Config) BySlashCommand(cmd string) (Cluster, bool) {
	cl, ok := c.bySlashCommand[cmd]
	return cl, ok
}

// Validate は整合性チェックと lookup map の初期化を行う。
// 検証ルール:
//   - clusters 1 件以上
//   - Controller.Namespace, Cluster.Env / Name / Slack.SlashCommand すべて空でない
//   - Provider.Type は eks / kubeconfig のいずれかで、対応する nested 設定が必須
//     (他 provider 設定が non-nil なら error)
//   - Env と SlashCommand は registry 内で一意
func (c *Config) Validate() error {
	if c.Controller.Namespace == "" {
		return errors.New("controller.namespace is required")
	}
	if len(c.Clusters) == 0 {
		return errors.New("cluster registry is empty")
	}

	byEnv := make(map[string]Cluster, len(c.Clusters))
	bySC := make(map[string]Cluster, len(c.Clusters))

	for i, cl := range c.Clusters {
		if cl.Env == "" {
			return fmt.Errorf("cluster[%d]: env is required", i)
		}
		if cl.Name == "" {
			return fmt.Errorf("cluster[%d] %q: name is required", i, cl.Env)
		}
		if cl.Slack.SlashCommand == "" {
			return fmt.Errorf("cluster[%d] %q: slack.slashCommand is required", i, cl.Env)
		}
		if err := validateProvider(cl); err != nil {
			return fmt.Errorf("cluster[%d] %q: %w", i, cl.Env, err)
		}
		if _, dup := byEnv[cl.Env]; dup {
			return fmt.Errorf("cluster registry duplicate env %q", cl.Env)
		}
		if _, dup := bySC[cl.Slack.SlashCommand]; dup {
			return fmt.Errorf("cluster registry duplicate slashCommand %q (env=%q)", cl.Slack.SlashCommand, cl.Env)
		}
		byEnv[cl.Env] = cl
		bySC[cl.Slack.SlashCommand] = cl
	}

	c.byEnv = byEnv
	c.bySlashCommand = bySC
	return nil
}

func validateProvider(c Cluster) error {
	switch c.Provider.Type {
	case ProviderTypeEKS:
		if c.Provider.EKS == nil {
			return errors.New("provider.eks is required when provider.type=eks")
		}
		if c.Provider.EKS.Region == "" {
			return errors.New("provider.eks.region is required")
		}
		if c.Provider.Kubeconfig != nil {
			return errors.New("provider.kubeconfig must be empty when provider.type=eks")
		}
	case ProviderTypeKubeconfig:
		if c.Provider.Kubeconfig == nil {
			return errors.New("provider.kubeconfig is required when provider.type=kubeconfig")
		}
		if c.Provider.Kubeconfig.Path == "" {
			return errors.New("provider.kubeconfig.path is required")
		}
		if c.Provider.EKS != nil {
			return errors.New("provider.eks must be empty when provider.type=kubeconfig")
		}
	case "":
		return errors.New("provider.type is required")
	default:
		return fmt.Errorf("unsupported provider.type %q", c.Provider.Type)
	}
	return nil
}

// Parse は YAML バイト列を *Config にパースする。Validate は呼ばないので呼び出し側で実行する。
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cluster registry yaml: %w", err)
	}
	return &cfg, nil
}

// Load は path のファイルを読み、Parse + Validate して *Config を返す。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cluster registry file %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("cluster registry file %s is empty", path)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
