package reload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	kedafake "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/murasame29/workload-knocker/internal/actuator/kedaprometheus"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/store"
)

// recMetrics records reload counters and deleted clusters on top of the real metrics.
type recMetrics struct {
	metrics.MetricsManager
	mu      sync.Mutex
	reloads map[string]int
	deleted []string
}

func newRecMetrics() *recMetrics {
	return &recMetrics{MetricsManager: metrics.New(), reloads: map[string]int{}}
}

func (r *recMetrics) IncConfigReload(target, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloads[target+"/"+result]++
}

func (r *recMetrics) DeleteCluster(env, cluster string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, env+"/"+cluster)
}

func (r *recMetrics) count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloads[key]
}

func registryYAML(frontend string, envs ...string) string {
	var b strings.Builder
	b.WriteString("controller: {namespace: workload-control}\nclusters:\n")
	for _, e := range envs {
		fmt.Fprintf(&b, "  - env: %s\n    name: %s-cluster\n    provider: {type: eks, eks: {region: ap-northeast-1}}\n    frontends: {%s: {slashCommand: /wk-%s}}\n", e, e, frontend, e)
	}
	return b.String()
}

func scaledObject(ns, name string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-so", Namespace: ns},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: name},
			Triggers:       []kedav1alpha1.ScaleTriggers{{Type: "prometheus", Metadata: map[string]string{"query": "workload_knocker_activation"}}},
		},
	}
}

func fakeFactory(m metrics.MetricsManager) store.ClusterFactory {
	return func(_ context.Context, c config.Cluster, ns string) (*store.ClusterStore, error) {
		lister := k8s.NewKEDAScaledObjectLister(kedafake.NewSimpleClientset(scaledObject("ns", "w-"+c.Env)))
		be := kedaprometheus.New(c, lister, nil, m)
		return store.NewClusterStore(c, be, be, k8s.NewConfigMapStateRepository(fake.NewClientset(), ns, c.Env)), nil
	}
}

type registryFixture struct {
	r    *Registry
	path string
	m    *recMetrics
	ms   *store.MultiStore
}

func newRegistryFixture(t *testing.T, initial string) *registryFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := newRecMetrics()
	stores := map[string]*store.ClusterStore{}
	for _, c := range reg.All() {
		cs, err := fakeFactory(m)(context.Background(), c, reg.Controller.Namespace)
		if err != nil {
			t.Fatal(err)
		}
		stores[c.Env] = cs
	}
	ms, err := store.NewMultiStoreFromClusterStores(reg, m, stores, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := &Registry{
		Path: path, Env: &config.Env{SlackAppToken: "a", SlackBotToken: "b"},
		Config: config.NewHolder(reg), Store: ms, Build: fakeFactory(m), Metrics: m,
	}
	r.Seed()
	return &registryFixture{r: r, path: path, m: m, ms: ms}
}

func (f *registryFixture) write(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(f.path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *registryFixture) envs() []string {
	var out []string
	for _, c := range f.r.Config.Load().All() {
		out = append(out, c.Env)
	}
	return out
}

func TestRegistryReload_ValidChangeIsApplied(t *testing.T) {
	t.Parallel()

	f := newRegistryFixture(t, registryYAML("slack", "stg"))
	f.write(t, registryYAML("slack", "stg", "dev"))
	f.r.Reload(context.Background())

	if got := strings.Join(f.envs(), ","); got != "stg,dev" {
		t.Errorf("live envs = %s", got)
	}
	if _, ok := f.r.Config.BySlashCommand("/wk-dev"); !ok {
		t.Error("new slash command must resolve through the live registry")
	}
	if len(f.ms.Namespaces("dev")) != 1 {
		t.Errorf("dev store not served: %v", f.ms.Namespaces("dev"))
	}
	if f.m.count("registry/success") != 1 || f.m.count("registry/error") != 0 {
		t.Errorf("counters = %v", f.m.reloads)
	}
}

func TestRegistryReload_InvalidChangeKeepsPreviousAndCounts(t *testing.T) {
	t.Parallel()

	f := newRegistryFixture(t, registryYAML("slack", "stg"))
	bad := []string{
		"clusters: [",
		registryYAML("slack", "stg", "stg"),
		"controller: {namespace: workload-control}\nclusters: []\n",
		"",
	}
	for _, body := range bad {
		f.write(t, body)
		f.r.Reload(context.Background())
	}

	if got := strings.Join(f.envs(), ","); got != "stg" {
		t.Errorf("live envs = %s, want the previous config", got)
	}
	if _, ok := f.r.Config.BySlashCommand("/wk-stg"); !ok {
		t.Error("previous slash command must still resolve")
	}
	if f.m.count("registry/error") != len(bad) || f.m.count("registry/success") != 0 {
		t.Errorf("counters = %v, want %d errors", f.m.reloads, len(bad))
	}

	f.write(t, registryYAML("slack", "stg"))
	os.Remove(f.path)
	f.r.Reload(context.Background())
	if f.m.count("registry/error") != len(bad)+1 {
		t.Errorf("a deleted file must count as an error: %v", f.m.reloads)
	}
}

func TestRegistryReload_RepeatedInvalidContentCountsOnce(t *testing.T) {
	t.Parallel()

	f := newRegistryFixture(t, registryYAML("slack", "stg"))
	f.write(t, "clusters: [")
	for range 3 {
		f.r.Reload(context.Background())
	}
	if got := f.m.count("registry/error"); got != 1 {
		t.Errorf("errors = %d, want 1 for identical invalid content", got)
	}
}

func TestRegistryReload_UnchangedContentIsNotAReload(t *testing.T) {
	t.Parallel()

	f := newRegistryFixture(t, registryYAML("slack", "stg"))
	f.r.Reload(context.Background())
	f.write(t, "# comment only\n"+registryYAML("slack", "stg"))
	f.r.Reload(context.Background())

	if len(f.m.reloads) != 0 {
		t.Errorf("counters = %v, want none", f.m.reloads)
	}
}

func TestRegistryReload_RemovesClusterAndItsSeries(t *testing.T) {
	t.Parallel()

	f := newRegistryFixture(t, registryYAML("slack", "stg", "dev"))
	f.write(t, registryYAML("slack", "stg"))
	f.r.Reload(context.Background())

	if got := strings.Join(f.envs(), ","); got != "stg" {
		t.Errorf("live envs = %s", got)
	}
	if _, ok := f.r.Config.BySlashCommand("/wk-dev"); ok {
		t.Error("removed cluster's slash command must stop resolving")
	}
	if f.ms.Namespaces("dev") != nil {
		t.Error("removed cluster's store must be dropped")
	}
	if len(f.m.deleted) != 1 || f.m.deleted[0] != "dev/dev-cluster" {
		t.Errorf("deleted series = %v", f.m.deleted)
	}
}

func TestRegistryReload_ChangeNeedingRestartIsRejected(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"controller namespace": strings.Replace(registryYAML("slack", "stg"), "workload-control", "elsewhere", 1),
	}
	for name, body := range cases {
		f := newRegistryFixture(t, registryYAML("slack", "stg"))
		f.write(t, body)
		f.r.Reload(context.Background())
		if f.m.count("registry/error") != 1 || f.r.Config.Load().Controller.Namespace != "workload-control" {
			t.Errorf("%s: counters = %v, want rejected", name, f.m.reloads)
		}
	}
}

func TestRestartRequired(t *testing.T) {
	t.Parallel()

	load := func(body string) *config.Config {
		cfg, err := config.Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cur := load(registryYAML("slack", "stg"))
	env := &config.Env{SlackAppToken: "a", SlackBotToken: "b"}

	if err := RestartRequired(cur, load(registryYAML("slack", "stg", "dev")), env); err != nil {
		t.Errorf("adding a cluster must not need a restart: %v", err)
	}
	if err := RestartRequired(cur, load(strings.Replace(registryYAML("slack", "stg"), "workload-control", "x", 1)), env); err == nil || !strings.Contains(err.Error(), "controller.namespace") {
		t.Errorf("namespace change: err = %v", err)
	}
	// A frontend that was not enabled at startup needs a restart (tokens, connection).
	noFrontends := &config.Config{Controller: cur.Controller}
	if err := RestartRequired(noFrontends, cur, env); err == nil || !strings.Contains(err.Error(), "newly enabled") {
		t.Errorf("new frontend: err = %v", err)
	}
	if err := RestartRequired(cur, cur, &config.Env{}); err == nil || !strings.Contains(err.Error(), "SLACK_APP_TOKEN") {
		t.Errorf("missing tokens: err = %v", err)
	}
	// Discord is a known frontend too: enabling it live needs a restart, changing its commands does not.
	discord := func(cmd string) *config.Config {
		return load("controller: {namespace: workload-control}\nclusters:\n  - env: stg\n    name: stg-cluster\n    provider: {type: eks, eks: {region: ap-northeast-1}}\n    frontends: {slack: {slashCommand: /wk-stg}, discord: {command: " + cmd + "}}\n")
	}
	if err := RestartRequired(cur, discord("knock"), env); err == nil || !strings.Contains(err.Error(), "discord") {
		t.Errorf("enabling Discord live: err = %v", err)
	}
	denv := &config.Env{SlackAppToken: "a", SlackBotToken: "b", DiscordBotToken: "d"}
	if err := RestartRequired(discord("knock"), discord("knock2"), denv); err != nil {
		t.Errorf("changing a Discord command must not need a restart: %v", err)
	}
}

func TestAuthzReload(t *testing.T) {
	t.Parallel()

	const allowAPI = "version: 1\nrules:\n  - {env: '*', namespaces: ['*'], subjects: {api: true}}\n"
	path := filepath.Join(t.TempDir(), "authz.yaml")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(allowAPI)
	first, err := authz.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	holder := authz.NewHolder(first)
	m := newRecMetrics()
	a := &Authz{Path: path, Policy: holder, Metrics: m}
	a.Seed()
	allowed := func() bool { return holder.Allowed(authz.APISubject, "stg", "ns", "w", authz.ActionStop) }
	ctx := context.Background()

	a.Reload(ctx)
	if m.count("authz/success")+m.count("authz/error") != 0 {
		t.Errorf("unchanged file must not count: %v", m.reloads)
	}

	write("version: 1\n")
	a.Reload(ctx)
	if allowed() || m.count("authz/success") != 1 {
		t.Errorf("deny-all policy not applied: allowed=%v counters=%v", allowed(), m.reloads)
	}

	write("version: 1\nrules: [{env: x}]\n")
	a.Reload(ctx)
	if allowed() || m.count("authz/error") != 1 {
		t.Errorf("invalid policy must keep the previous one: allowed=%v counters=%v", allowed(), m.reloads)
	}

	write(allowAPI)
	a.Reload(ctx)
	if !allowed() || m.count("authz/success") != 2 {
		t.Errorf("valid policy not applied again: counters=%v", m.reloads)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	a.Reload(ctx)
	if !allowed() || m.count("authz/error") != 2 {
		t.Errorf("deleted file must keep the previous policy: counters=%v", m.reloads)
	}
}

func TestAuthzReload_EnablesAuthorizationWhenFileAppears(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "authz.yaml")
	holder := authz.NewHolder(nil)
	m := newRecMetrics()
	a := &Authz{Path: path, Policy: holder, Metrics: m}
	a.Seed()

	if err := os.WriteFile(path, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Reload(context.Background())
	if holder.Allowed(authz.APISubject, "stg", "ns", "w", authz.ActionStart) {
		t.Error("policy that appeared at runtime must be enforced")
	}
}

func TestRegistryReload_NotifyChannelAppliesWithoutRestart(t *testing.T) {
	t.Parallel()

	slackWith := func(extra string) string {
		return "controller: {namespace: workload-control}\nclusters:\n  - env: stg\n    name: stg-cluster\n    provider: {type: eks, eks: {region: ap-northeast-1}}\n    frontends: {slack: {slashCommand: /wk-stg" + extra + "}}\n"
	}
	f := newRegistryFixture(t, slackWith(""))
	channel := func() string {
		cl, _ := f.r.Config.ByEnv("stg")
		return cl.NotifyChannel(config.FrontendSlack)
	}
	if channel() != "" {
		t.Fatalf("channel = %q before reload", channel())
	}

	f.write(t, slackWith(", notifyChannel: C0123ABCD"))
	f.r.Reload(context.Background())
	if channel() != "C0123ABCD" || f.m.count("registry/success") != 1 {
		t.Errorf("channel = %q counters = %v, want the new channel applied live", channel(), f.m.reloads)
	}

	f.write(t, slackWith(", notifyChannel: general"))
	f.r.Reload(context.Background())
	if channel() != "C0123ABCD" || f.m.count("registry/error") != 1 {
		t.Errorf("an invalid channel must keep the previous config: channel=%q counters=%v", channel(), f.m.reloads)
	}
}
