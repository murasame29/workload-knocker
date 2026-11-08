package config

import (
	"strings"
	"testing"
)

func validClusters() []Cluster {
	return []Cluster{
		{
			Env:      "stg",
			Name:     "example-staging",
			Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}},
			Slack:    ClusterSlack{SlashCommand: "/workload-knocker-stg"},
		},
		{
			Env:      "stg-us",
			Name:     "example-staging",
			Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "us-west-2"}},
			Slack:    ClusterSlack{SlashCommand: "/workload-knocker-stg-us"},
		},
		{
			Env:      "dev",
			Name:     "example-development",
			Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1", AssumeRoleArn: "arn:aws:iam::123456789012:role/x"}},
			Slack:    ClusterSlack{SlashCommand: "/workload-knocker-dev"},
		},
	}
}

func validConfig() *Config {
	return &Config{
		Controller: Controller{Namespace: "workload-knocker"},
		Clusters:   validClusters(),
	}
}

func TestValidate_Valid(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.Controller.Namespace != "workload-knocker" {
		t.Errorf("Controller.Namespace = %q", cfg.Controller.Namespace)
	}

	if got := len(cfg.All()); got != 3 {
		t.Fatalf("All(): got %d, want 3", got)
	}

	stg, ok := cfg.ByEnv("stg")
	if !ok {
		t.Fatalf("ByEnv(stg): not found")
	}
	if stg.Name != "example-staging" {
		t.Errorf("stg.Name: got %q", stg.Name)
	}
	if stg.Provider.EKS == nil || stg.Provider.EKS.Region != "ap-northeast-1" {
		t.Errorf("stg.Provider.EKS.Region: got %+v", stg.Provider.EKS)
	}

	if _, ok := cfg.ByEnv("nonexistent"); ok {
		t.Errorf("ByEnv(nonexistent): expected not found")
	}

	dev, ok := cfg.BySlashCommand("/workload-knocker-dev")
	if !ok {
		t.Fatalf("BySlashCommand(dev): not found")
	}
	if dev.Env != "dev" {
		t.Errorf("dev.Env: got %q, want %q", dev.Env, "dev")
	}
	if dev.Provider.EKS == nil || dev.Provider.EKS.AssumeRoleArn == "" {
		t.Errorf("dev.Provider.EKS.AssumeRoleArn should not be empty")
	}

	// All() should return a copy
	all := cfg.All()
	all[0].Env = "MUTATED"
	if got := cfg.All()[0].Env; got != "stg" {
		t.Errorf("Config leaked internal slice: got %q after external mutation", got)
	}
}

func TestValidate_EmptyClusters(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Clusters = nil
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty error, got %v", err)
	}

	cfg = validConfig()
	cfg.Clusters = []Cluster{}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty error, got %v", err)
	}
}

func TestValidate_MissingController(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Controller.Namespace = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "controller.namespace") {
		t.Errorf("expected controller.namespace error, got %v", err)
	}
}

func TestValidate_MissingFieldError(t *testing.T) {
	t.Parallel()

	base := Cluster{
		Env:      "stg",
		Name:     "example-staging",
		Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}},
		Slack:    ClusterSlack{SlashCommand: "/workload-knocker-stg"},
	}

	cases := []struct {
		name      string
		mutate    func(*Cluster)
		errSubstr string
	}{
		{"missing env", func(c *Cluster) { c.Env = "" }, "env is required"},
		{"missing name", func(c *Cluster) { c.Name = "" }, "name is required"},
		{"missing region", func(c *Cluster) { c.Provider.EKS.Region = "" }, "provider.eks.region is required"},
		{"missing slashCommand", func(c *Cluster) { c.Slack.SlashCommand = "" }, "slack.slashCommand is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := base
			c.Provider.EKS = &ProviderEKS{Region: base.Provider.EKS.Region}
			tc.mutate(&c)
			cfg := &Config{
				Controller: Controller{Namespace: "workload-knocker"},
				Clusters:   []Cluster{c},
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error: got %q, want substring %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

func TestValidate_DuplicateEnvError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Controller: Controller{Namespace: "workload-knocker"},
		Clusters: []Cluster{
			{Env: "stg", Name: "a", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}}, Slack: ClusterSlack{SlashCommand: "/cmd-a"}},
			{Env: "stg", Name: "b", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "us-west-2"}}, Slack: ClusterSlack{SlashCommand: "/cmd-b"}},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate env") {
		t.Errorf("expected duplicate env error, got %v", err)
	}
}

func TestValidate_DuplicateSlashCommandError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Controller: Controller{Namespace: "workload-knocker"},
		Clusters: []Cluster{
			{Env: "a", Name: "a", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}}, Slack: ClusterSlack{SlashCommand: "/cmd"}},
			{Env: "b", Name: "b", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "us-west-2"}}, Slack: ClusterSlack{SlashCommand: "/cmd"}},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate slashCommand") {
		t.Errorf("expected duplicate slashCommand error, got %v", err)
	}
}
