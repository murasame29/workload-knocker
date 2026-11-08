package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
    slack:
      slashCommand: /workload-knocker-stg
  - env: dev
    name: example-development
    provider:
      type: eks
      eks:
        region: ap-northeast-1
        assumeRoleArn: arn:aws:iam::123456789012:role/x
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
