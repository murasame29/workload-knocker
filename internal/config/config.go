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
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider type identifiers.
const (
	ProviderTypeEKS        = "eks"
	ProviderTypeKubeconfig = "kubeconfig"
)

// Config is the root Cluster Registry schema plus the validated lookup.
// It is immutable once it passes Validate / Load; hot reload swaps a whole Config (see Holder).
type Config struct {
	Controller Controller `yaml:"controller"`
	Clusters   []Cluster  `yaml:"clusters"`

	byEnv          map[string]Cluster
	bySlashCommand map[string]Cluster
	byDiscordCmd   map[string]Cluster
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

	// Actuator selects how workloads are discovered and started or stopped; empty means keda-prometheus.
	Actuator string `yaml:"actuator,omitempty"`

	Provider  ClusterProvider  `yaml:"provider"`
	Frontends ClusterFrontends `yaml:"frontends"`

	// LegacySlack catches the removed top-level slack key so Validate can point to frontends.slack.
	LegacySlack *yaml.Node `yaml:"slack,omitempty"`
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

// Actuator identifiers. An empty Cluster.Actuator selects ActuatorKEDAPrometheus.
const (
	ActuatorKEDAPrometheus = "keda-prometheus"
)

// ActuatorNames lists every supported actuator; add new ones here and in internal/actuator/factory.
var ActuatorNames = []string{ActuatorKEDAPrometheus}

// Frontend identifiers.
const (
	FrontendSlack   = "slack"
	FrontendDiscord = "discord"
)

// ClusterFrontends holds the per-frontend settings of a cluster; a nil entry means not configured.
type ClusterFrontends struct {
	Slack   *SlackFrontend   `yaml:"slack,omitempty"`
	Discord *DiscordFrontend `yaml:"discord,omitempty"`
}

// SlackFrontend holds Slack settings. SlashCommand is unique in the registry.
type SlackFrontend struct {
	SlashCommand string `yaml:"slashCommand"`
	// NotifyChannel is an optional channel ID that receives public start, ready and stop messages.
	NotifyChannel string `yaml:"notifyChannel,omitempty"`
}

// FrontendEnabled reports whether at least one cluster configures the named frontend.
func (c *Config) FrontendEnabled(name string) bool {
	for _, cl := range c.Clusters {
		if name == FrontendSlack && cl.Frontends.Slack != nil {
			return true
		}
		if name == FrontendDiscord && cl.Frontends.Discord != nil {
			return true
		}
	}
	return false
}

// FrontendNames lists every known frontend; add new frontends here so hot reload can tell a
// newly enabled one (restart required) from a change that can be applied live.
var FrontendNames = []string{FrontendSlack, FrontendDiscord}

// EnabledFrontends returns the names of the frontends at least one cluster configures.
func (c *Config) EnabledFrontends() []string {
	var out []string
	for _, n := range FrontendNames {
		if c.FrontendEnabled(n) {
			out = append(out, n)
		}
	}
	return out
}

// NotifyChannel returns the notify channel ID configured for the frontend, or "" when none.
func (c Cluster) NotifyChannel(frontend string) string {
	switch {
	case frontend == FrontendSlack && c.Frontends.Slack != nil:
		return c.Frontends.Slack.NotifyChannel
	case frontend == FrontendDiscord && c.Frontends.Discord != nil:
		return c.Frontends.Discord.NotifyChannel
	}
	return ""
}

// ActuatorType returns the configured actuator, or the default when unset.
func (c Cluster) ActuatorType() string {
	if c.Actuator == "" {
		return ActuatorKEDAPrometheus
	}
	return c.Actuator
}

// SameBackend reports whether o reaches the same cluster the same way (name, provider and actuator).
func (c Cluster) SameBackend(o Cluster) bool {
	return c.Name == o.Name && c.ActuatorType() == o.ActuatorType() && reflect.DeepEqual(c.Provider, o.Provider)
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
//   - Controller.Namespace, Cluster.Env / Name are non-empty
//   - at least one frontend is configured per cluster, and its required fields are set
//   - the removed top-level slack key is rejected with a pointer to frontends.slack
//   - Actuator, when set, is one of ActuatorNames
//   - Provider.Type is eks or kubeconfig with its nested setting present (other providers must be nil)
//   - Env and each frontend's command are unique in the registry
func (c *Config) Validate() error {
	if c.Controller.Namespace == "" {
		return errors.New("controller.namespace is required")
	}
	if len(c.Clusters) == 0 {
		return errors.New("cluster registry is empty")
	}

	byEnv := make(map[string]Cluster, len(c.Clusters))
	bySC := make(map[string]Cluster, len(c.Clusters))
	byDC := make(map[string]Cluster)

	for i, cl := range c.Clusters {
		if cl.Env == "" {
			return fmt.Errorf("cluster[%d]: env is required", i)
		}
		if cl.Name == "" {
			return fmt.Errorf("cluster[%d] %q: name is required", i, cl.Env)
		}
		if cl.LegacySlack != nil {
			return fmt.Errorf("cluster[%d] %q: slack is no longer supported; move it to frontends.slack (frontends.slack.slashCommand)", i, cl.Env)
		}
		if cl.Frontends.Slack == nil && cl.Frontends.Discord == nil {
			return fmt.Errorf("cluster[%d] %q: at least one frontend is required (frontends.slack.slashCommand or frontends.discord.command)", i, cl.Env)
		}
		if cl.Frontends.Slack != nil && cl.Frontends.Slack.SlashCommand == "" {
			return fmt.Errorf("cluster[%d] %q: frontends.slack.slashCommand is required", i, cl.Env)
		}
		if cl.Frontends.Slack != nil {
			if err := validateSlackChannel(cl.Frontends.Slack.NotifyChannel); err != nil {
				return fmt.Errorf("cluster[%d] %q: frontends.slack.notifyChannel: %w", i, cl.Env, err)
			}
		}
		if cl.Frontends.Discord != nil {
			if err := validateDiscordCommand(cl.Frontends.Discord.Command); err != nil {
				return fmt.Errorf("cluster[%d] %q: frontends.discord.command: %w", i, cl.Env, err)
			}
			if err := validateDiscordChannel(cl.Frontends.Discord.NotifyChannel); err != nil {
				return fmt.Errorf("cluster[%d] %q: frontends.discord.notifyChannel: %w", i, cl.Env, err)
			}
		}
		if !slices.Contains(append([]string{""}, ActuatorNames...), cl.Actuator) {
			return fmt.Errorf("cluster[%d] %q: unsupported actuator %q (supported: %s)", i, cl.Env, cl.Actuator, strings.Join(ActuatorNames, ", "))
		}
		if err := validateProvider(cl); err != nil {
			return fmt.Errorf("cluster[%d] %q: %w", i, cl.Env, err)
		}
		if _, dup := byEnv[cl.Env]; dup {
			return fmt.Errorf("cluster registry duplicate env %q", cl.Env)
		}
		if cl.Frontends.Slack != nil {
			sc := cl.Frontends.Slack.SlashCommand
			if _, dup := bySC[sc]; dup {
				return fmt.Errorf("cluster registry duplicate slashCommand %q (env=%q)", sc, cl.Env)
			}
			bySC[sc] = cl
		}
		if cl.Frontends.Discord != nil {
			dc := cl.Frontends.Discord.Command
			if _, dup := byDC[dc]; dup {
				return fmt.Errorf("cluster registry duplicate discord command %q (env=%q)", dc, cl.Env)
			}
			byDC[dc] = cl
		}
		byEnv[cl.Env] = cl
	}

	c.byEnv = byEnv
	c.bySlashCommand = bySC
	c.byDiscordCmd = byDC
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
	return ParseValidated(path, data)
}

// ParseValidated Parses and Validates registry bytes read from path.
func ParseValidated(path string, data []byte) (*Config, error) {
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
