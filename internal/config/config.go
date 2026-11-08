// Package config handles workload-knocker configuration:
//
//   - process settings from environment variables (env.go: Env, LoadEnv)
//   - the Cluster Registry YAML schema and lookup (config.go: Config, Load)
//
// The Cluster Registry is read from a file (a read-only mounted ConfigMap in production),
// so this package does not depend on the K8s API.
package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Provider type identifiers.
const (
	ProviderTypeEKS        = "eks"
	ProviderTypeKubeconfig = "kubeconfig"
)

// Config is the root Cluster Registry schema plus the validated lookup.
// It is immutable once it passes Validate / Load (no hot-reload).
type Config struct {
	Controller Controller `yaml:"controller"`
	Clusters   []Cluster  `yaml:"clusters"`

	byEnv          map[string]Cluster
	bySlashCommand map[string]Cluster
}

// Controller is the configuration of the cluster the workload-knocker Pod runs in.
type Controller struct {
	// Namespace holds the state ConfigMap (workload-state).
	Namespace string `yaml:"namespace"`
}

// Cluster is the configuration of one managed cluster.
type Cluster struct {
	// Env is the env identifier (e.g. stg, stg-us, dev), unique in the registry; also used as metrics label / Modal key.
	Env string `yaml:"env"`

	// Name is the cluster name, used as the metrics cluster label and the EKS DescribeCluster argument.
	Name string `yaml:"name"`

	Provider ClusterProvider `yaml:"provider"`
	Slack    ClusterSlack    `yaml:"slack"`
}

// ClusterProvider is a discriminated union describing how to connect to a cluster.
// Only the field matching Type is used.
type ClusterProvider struct {
	Type       string              `yaml:"type"`
	EKS        *ProviderEKS        `yaml:"eks,omitempty"`
	Kubeconfig *ProviderKubeconfig `yaml:"kubeconfig,omitempty"`
}

// ProviderEKS is the AWS EKS connection setting (IRSA + optional cross-account AssumeRole).
type ProviderEKS struct {
	Region        string `yaml:"region"`
	AssumeRoleArn string `yaml:"assumeRoleArn,omitempty"`
}

// ProviderKubeconfig is the kubeconfig file connection setting.
type ProviderKubeconfig struct {
	Path    string `yaml:"path"`
	Context string `yaml:"context,omitempty"`
}

// ClusterSlack holds Slack settings. SlashCommand is unique in the registry.
type ClusterSlack struct {
	SlashCommand string `yaml:"slashCommand"`
}

// All returns a copy of all registered clusters.
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

// Validate checks consistency and initializes the lookup map. Rules:
//   - at least one cluster
//   - Controller.Namespace, Cluster.Env / Name / Slack.SlashCommand are non-empty
//   - Provider.Type is eks or kubeconfig with its nested setting present (other providers must be nil)
//   - Env and SlashCommand are unique in the registry
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

// Parse parses YAML bytes into *Config. It does not call Validate; the caller must.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cluster registry yaml: %w", err)
	}
	return &cfg, nil
}

// Load reads the file at path, then Parses and Validates it into *Config.
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
