package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeYAML writes a YAML file in a tempdir and returns its path.
func writeYAML(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoad_FromFile(t *testing.T) {
	t.Parallel()

	yamlBody := `
controller:
  namespace: workload-knocker
clusters:
  - env: stg
    name: example-staging
    provider:
      type: eks
      eks:
        region: ap-northeast-1
    frontends:
      slack:
        slashCommand: /workload-knocker-stg
  - env: dev
    name: example-development
    provider:
      type: eks
      eks:
        region: ap-northeast-1
        assumeRoleArn: arn:aws:iam::123456789012:role/x
    frontends:
      slack:
        slashCommand: /workload-knocker-dev
`
	path := writeYAML(t, "config.yaml", yamlBody)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := len(cfg.Clusters); got != 2 {
		t.Fatalf("Clusters: got %d, want 2", got)
	}
	if cfg.Controller.Namespace != "workload-knocker" {
		t.Errorf("Controller.Namespace: got %q", cfg.Controller.Namespace)
	}
	if cfg.Clusters[1].Env != "dev" {
		t.Errorf("Clusters[1].Env: got %q, want %q", cfg.Clusters[1].Env, "dev")
	}
	if cfg.Clusters[1].Provider.EKS == nil {
		t.Fatalf("Clusters[1].Provider.EKS is nil")
	}
	if cfg.Clusters[1].Provider.EKS.AssumeRoleArn != "arn:aws:iam::123456789012:role/x" {
		t.Errorf("Clusters[1].Provider.EKS.AssumeRoleArn: got %q", cfg.Clusters[1].Provider.EKS.AssumeRoleArn)
	}
}

func TestLoad_FileMissingError(t *testing.T) {
	t.Parallel()

	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil || !strings.Contains(err.Error(), "read cluster registry file") {
		t.Errorf("expected read error, got %v", err)
	}
}

func TestLoad_EmptyFileError(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, "empty.yaml", "")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("expected empty file error, got %v", err)
	}
}

func TestLoad_InvalidYamlError(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, "bad.yaml", "controller: [this is :: invalid yaml")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "parse cluster registry yaml") {
		t.Errorf("expected yaml parse error, got %v", err)
	}
}

func TestParse_InvalidYaml(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte("controller: [this is :: invalid yaml"))
	if err == nil || !strings.Contains(err.Error(), "parse cluster registry yaml") {
		t.Errorf("expected yaml parse error, got %v", err)
	}
}

func TestParse_Valid(t *testing.T) {
	t.Parallel()

	yamlBody := `
controller:
  namespace: workload-knocker
clusters:
  - env: stg
    name: stg-cluster
    provider:
      type: eks
      eks:
        region: ap-northeast-1
    frontends:
      slack:
        slashCommand: /cmd
`
	cfg, err := Parse([]byte(yamlBody))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Controller.Namespace != "workload-knocker" {
		t.Errorf("Controller.Namespace: got %q", cfg.Controller.Namespace)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Env != "stg" {
		t.Errorf("Clusters: got %+v", cfg.Clusters)
	}
}

func TestLoadEnv_OperationMode(t *testing.T) {
	t.Setenv("SLACK_APP_TOKEN", "x")
	t.Setenv("SLACK_BOT_TOKEN", "x")
	t.Setenv("OPERATION_MODE", "bogus")
	if _, err := LoadEnv(); err == nil || !strings.Contains(err.Error(), "full, scheduled-stop, off") {
		t.Fatalf("expected allowed values in error, got %v", err)
	}
	t.Setenv("OPERATION_MODE", "off")
	e, err := LoadEnv()
	if err != nil || e.OperationMode != "off" {
		t.Fatalf("got %v, %v", e, err)
	}
}

func TestLoadEnv_ReconcileInterval(t *testing.T) {
	e, err := LoadEnv()
	if err != nil || e.ReconcileInterval != time.Minute {
		t.Fatalf("default: got (%v, %v), want 1m", e, err)
	}

	t.Setenv("RECONCILE_INTERVAL", "0")
	if e, err = LoadEnv(); err != nil || e.ReconcileInterval != 0 {
		t.Fatalf("0: got (%v, %v), want disabled", e, err)
	}

	t.Setenv("RECONCILE_INTERVAL", "-1s")
	if _, err = LoadEnv(); err == nil {
		t.Fatal("negative interval: want error")
	}
}

func TestLoadEnv_RolloutDefaults(t *testing.T) {
	e, err := LoadEnv()
	if err != nil || e.RolloutPollInterval != 5*time.Second || e.RolloutWatchTimeout != 10*time.Minute {
		t.Fatalf("got %+v, %v; want 5s / 10m", e, err)
	}
}

func TestLoadEnv_RolloutValidation(t *testing.T) {
	cases := []struct{ name, interval, timeout string }{
		{"timeout at the Discord token lifetime", "5s", "15m"},
		{"timeout above the Discord token lifetime", "5s", "20m"},
		{"interval below 1s", "100ms", "10m"},
		{"timeout not above interval", "30s", "30s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ROLLOUT_POLL_INTERVAL", c.interval)
			t.Setenv("ROLLOUT_WATCH_TIMEOUT", c.timeout)
			if _, err := LoadEnv(); err == nil || !strings.Contains(err.Error(), "ROLLOUT_") {
				t.Fatalf("got %v, want a ROLLOUT_* error", err)
			}
		})
	}

	t.Setenv("ROLLOUT_POLL_INTERVAL", "2s")
	t.Setenv("ROLLOUT_WATCH_TIMEOUT", "14m")
	if _, err := LoadEnv(); err != nil {
		t.Fatalf("valid values rejected: %v", err)
	}
}
