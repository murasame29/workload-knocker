package config

import (
	"strings"
	"testing"
)

func validClusters() []Cluster {
	return []Cluster{
		{
			Env:       "stg",
			Name:      "example-staging",
			Provider:  ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}},
			Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/workload-knocker-stg"}},
		},
		{
			Env:       "stg-us",
			Name:      "example-staging",
			Provider:  ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "us-west-2"}},
			Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/workload-knocker-stg-us"}},
		},
		{
			Env:       "dev",
			Name:      "example-development",
			Provider:  ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1", AssumeRoleArn: "arn:aws:iam::123456789012:role/x"}},
			Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/workload-knocker-dev"}},
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
		Env:       "stg",
		Name:      "example-staging",
		Provider:  ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}},
		Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/workload-knocker-stg"}},
	}

	cases := []struct {
		name      string
		mutate    func(*Cluster)
		errSubstr string
	}{
		{"missing env", func(c *Cluster) { c.Env = "" }, "env is required"},
		{"missing name", func(c *Cluster) { c.Name = "" }, "name is required"},
		{"missing region", func(c *Cluster) { c.Provider.EKS.Region = "" }, "provider.eks.region is required"},
		{"missing slashCommand", func(c *Cluster) { c.Frontends.Slack.SlashCommand = "" }, "frontends.slack.slashCommand is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := base
			c.Provider.EKS = &ProviderEKS{Region: base.Provider.EKS.Region}
			c.Frontends.Slack = &SlackFrontend{SlashCommand: base.Frontends.Slack.SlashCommand}
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

func TestValidate_EnvFormat(t *testing.T) {
	t.Parallel()

	for _, env := range []string{"stg us", "stg/us", "ステージング"} {
		cfg := &Config{
			Controller: Controller{Namespace: "workload-knocker"},
			Clusters: []Cluster{
				{Env: env, Name: "a", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}}, Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/cmd-a"}}},
			},
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "env may contain only") {
			t.Errorf("env %q: got %v, want a format error", env, err)
		}
	}
}

func TestValidate_DuplicateEnvError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Controller: Controller{Namespace: "workload-knocker"},
		Clusters: []Cluster{
			{Env: "stg", Name: "a", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}}, Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/cmd-a"}}},
			{Env: "stg", Name: "b", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "us-west-2"}}, Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/cmd-b"}}},
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
			{Env: "a", Name: "a", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}}, Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/cmd"}}},
			{Env: "b", Name: "b", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "us-west-2"}}, Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/cmd"}}},
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate slashCommand") {
		t.Errorf("expected duplicate slashCommand error, got %v", err)
	}
}

func TestValidate_NoFrontendError(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Controller: Controller{Namespace: "workload-knocker"},
		Clusters: []Cluster{
			{Env: "a", Name: "a", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}}},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "at least one frontend is required") {
		t.Errorf("error: got %v, want at-least-one-frontend error", err)
	}
}

func TestParse_LegacySlackKeyRejectedWithHint(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
controller:
  namespace: workload-knocker
clusters:
  - env: stg
    name: stg-cluster
    provider:
      type: eks
      eks:
        region: ap-northeast-1
    slack:
      slashCommand: /cmd
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "frontends.slack") {
		t.Errorf("error: got %v, want hint mentioning frontends.slack", err)
	}
}

func TestFrontendEnabled(t *testing.T) {
	t.Parallel()

	cl := Cluster{Env: "a", Name: "a", Provider: ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}}}
	cfg := &Config{Clusters: []Cluster{cl}}
	if cfg.FrontendEnabled(FrontendSlack) {
		t.Error("FrontendEnabled(slack) = true with no frontend configured")
	}
	cl.Frontends.Slack = &SlackFrontend{SlashCommand: "/cmd"}
	cfg = &Config{Clusters: []Cluster{cl}}
	if !cfg.FrontendEnabled(FrontendSlack) {
		t.Error("FrontendEnabled(slack) = false with slack configured")
	}
}

func TestEnvValidateFrontends(t *testing.T) {
	t.Parallel()

	cl := Cluster{Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/cmd"}}}
	reg := &Config{Clusters: []Cluster{cl}}
	if err := (&Env{}).ValidateFrontends(reg); err == nil {
		t.Error("missing Slack tokens with Slack enabled: want error")
	}
	if err := (&Env{SlackAppToken: "a", SlackBotToken: "b"}).ValidateFrontends(reg); err != nil {
		t.Errorf("tokens set: %v", err)
	}
	if err := (&Env{}).ValidateFrontends(&Config{}); err != nil {
		t.Errorf("Slack disabled: %v", err)
	}
}

func TestValidate_Actuator(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		actuator string
		wantErr  string
	}{
		{actuator: ""},
		{actuator: ActuatorKEDAPrometheus},
		{actuator: ActuatorKEDAExternal},
		{actuator: ActuatorDirect},
		{actuator: "nope", wantErr: `unsupported actuator "nope"`},
	} {
		cfg := validConfig()
		cfg.Clusters[0].Actuator = tc.actuator
		err := cfg.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("actuator %q: %v", tc.actuator, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("actuator %q: error = %v, want %q", tc.actuator, err, tc.wantErr)
		}
	}
}

func TestCluster_SameBackend(t *testing.T) {
	t.Parallel()

	base := validClusters()[0]
	explicit := base
	explicit.Actuator = ActuatorKEDAPrometheus
	other := base
	other.Actuator = "other"
	if !base.SameBackend(explicit) {
		t.Error("empty and explicit default actuator must be the same backend")
	}
	if base.SameBackend(other) {
		t.Error("a different actuator must be a different backend")
	}
}
