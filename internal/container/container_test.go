package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/server"
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: t
clusters:
  - name: t
    cluster: {server: "https://127.0.0.1:1"}
contexts:
  - name: t
    context: {cluster: t, user: t}
users:
  - name: t
    user: {token: x}
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildServer_ResolvesTheWholeGraph(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := writeFile(t, dir, "kubeconfig", testKubeconfig)
	registry := writeFile(t, dir, "config.yaml", `
controller: {namespace: workload-control}
clusters:
  - env: local
    name: kind-local
    provider: {type: kubeconfig, kubeconfig: {path: `+kubeconfig+`}}
    frontends: {slack: {slashCommand: /wk-local}}
`)
	policy := writeFile(t, dir, "authz.yaml", "version: 1\nrules:\n  - {env: '*', namespaces: ['*'], subjects: {api: true}}\n")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	cfg := &config.Env{
		HTTPPort: 8080, ClusterConfigPath: registry, AuthzConfigPath: policy, APIToken: "t",
		SlackAppToken: "xapp-1-test", SlackBotToken: "xoxb-test",
	}
	c, err := BuildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Invoke(func(m *server.Manager) {
		if m == nil {
			t.Error("nil manager")
		}
	}); err != nil {
		t.Fatalf("resolving the manager: %v", err)
	}
}

func TestBuildServer_ExplicitAuthzPathMustParse(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := writeFile(t, dir, "kubeconfig", testKubeconfig)
	registry := writeFile(t, dir, "config.yaml", `
controller: {namespace: workload-control}
clusters:
  - env: local
    name: kind-local
    provider: {type: kubeconfig, kubeconfig: {path: `+kubeconfig+`}}
    frontends: {slack: {slashCommand: /wk-local}}
`)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	for name, path := range map[string]string{
		"missing file": filepath.Join(dir, "nope.yaml"),
		"invalid file": writeFile(t, dir, "bad.yaml", "version: 7\n"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Env{ClusterConfigPath: registry, AuthzConfigPath: path, SlackAppToken: "xapp-1-test", SlackBotToken: "xoxb-test"}
			c, err := BuildServer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = c.Invoke(func(*server.Manager) {})
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("err = %v, want startup failure naming %s", err, path)
			}
		})
	}
}

func TestNewAuthzHolder_UnsetAndNoDefaultFileDisablesAuthorization(t *testing.T) {
	if _, err := os.Stat("/etc/workload-knocker/authz.yaml"); err == nil {
		t.Skip("default policy file exists on this host")
	}
	h, err := newAuthzHolder(&config.Env{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Policy() != nil {
		t.Error("policy must be nil (authorization disabled)")
	}
}

func TestBuildServer_DiscordFrontendResolves(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := writeFile(t, dir, "kubeconfig", testKubeconfig)
	registry := writeFile(t, dir, "config.yaml", `
controller: {namespace: workload-control}
clusters:
  - env: local
    name: kind-local
    provider: {type: kubeconfig, kubeconfig: {path: `+kubeconfig+`}}
    frontends: {discord: {command: wk-local}}
`)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	cfg := &config.Env{HTTPPort: 8080, ClusterConfigPath: registry, AuthzConfigPath: writeFile(t, dir, "authz.yaml", "version: 1\ndefault: allow\n"), APIToken: "t", DiscordBotToken: "test-token"}
	c, err := BuildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Invoke(func(fs []frontend.Frontend) {
		if len(fs) != 1 || fs[0].Name() != "discord" {
			t.Errorf("frontends = %v, want only discord", fs)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNewExternalScaler_OnlyWhenAClusterUsesIt(t *testing.T) {
	cfg := &config.Env{ExternalScalerPort: 9090, ExternalScalerAddressMatch: "workload-knocker"}
	reg := func(actuator string) *config.Config {
		return &config.Config{Clusters: []config.Cluster{{Env: "stg", Name: "c", Actuator: actuator}}}
	}

	if s, err := newExternalScaler(cfg, reg("")); err != nil || s != nil {
		t.Errorf("default actuator: server = %v, err = %v, want none", s, err)
	}
	if s, err := newExternalScaler(cfg, reg(config.ActuatorKEDAExternal)); err != nil || s == nil {
		t.Errorf("keda-external: server = %v, err = %v, want a server", s, err)
	}
	bad := &config.Env{ExternalScalerPort: 9090, ExternalScalerTLSCert: "c.pem"}
	if _, err := newExternalScaler(bad, reg(config.ActuatorKEDAExternal)); err == nil {
		t.Error("cert without key was accepted")
	}
}
