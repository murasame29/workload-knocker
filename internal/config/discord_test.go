package config

import (
	"strings"
	"testing"
)

func discordCluster(env, cmd string) Cluster {
	return Cluster{
		Env:       env,
		Name:      env,
		Provider:  ClusterProvider{Type: ProviderTypeEKS, EKS: &ProviderEKS{Region: "ap-northeast-1"}},
		Frontends: ClusterFrontends{Discord: &DiscordFrontend{Command: cmd}},
	}
}

func TestValidate_DiscordCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, cmd, errSubstr string
	}{
		{"valid", "knock-stg_1", ""},
		{"empty", "", "is required"},
		{"leading slash", "/knock", "without a leading slash"},
		{"uppercase", "Knock", "lowercase"},
		{"space", "knock stg", "lowercase"},
		{"max length", strings.Repeat("a", 32), ""},
		{"too long", strings.Repeat("a", 33), "at most 32"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Controller: Controller{Namespace: "wk"}, Clusters: []Cluster{discordCluster("stg", tc.cmd)}}
			err := cfg.Validate()
			switch {
			case tc.errSubstr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.errSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.errSubstr)):
				t.Fatalf("error = %v, want substring %q", err, tc.errSubstr)
			}
		})
	}
}

func TestValidate_DiscordDuplicateAndLookup(t *testing.T) {
	t.Parallel()

	dup := &Config{Controller: Controller{Namespace: "wk"}, Clusters: []Cluster{discordCluster("a", "knock"), discordCluster("b", "knock")}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate discord command") {
		t.Errorf("error = %v, want duplicate discord command", err)
	}

	both := discordCluster("a", "knock-a")
	both.Frontends.Slack = &SlackFrontend{SlashCommand: "/knock-a"}
	cfg := &Config{Controller: Controller{Namespace: "wk"}, Clusters: []Cluster{both, discordCluster("b", "knock-b")}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cl, ok := cfg.ByDiscordCommand("knock-b"); !ok || cl.Env != "b" {
		t.Errorf("ByDiscordCommand(knock-b) = %+v, %v", cl, ok)
	}
	if _, ok := cfg.ByDiscordCommand("knock-c"); ok {
		t.Error("unknown command resolved")
	}
	if _, ok := cfg.BySlashCommand("/knock-a"); !ok {
		t.Error("slack lookup lost when discord is also configured")
	}
	if !cfg.FrontendEnabled(FrontendDiscord) || !cfg.FrontendEnabled(FrontendSlack) {
		t.Error("FrontendEnabled must report both frontends")
	}
}

func TestParse_DiscordFrontend(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
controller:
  namespace: wk
clusters:
  - env: stg
    name: stg
    provider:
      type: eks
      eks:
        region: ap-northeast-1
    frontends:
      discord:
        command: knock-stg
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Clusters[0].Frontends.Discord.Command != "knock-stg" {
		t.Errorf("command = %q", cfg.Clusters[0].Frontends.Discord.Command)
	}
}

func TestValidateFrontends_DiscordToken(t *testing.T) {
	t.Parallel()

	reg := &Config{Clusters: []Cluster{discordCluster("stg", "knock")}}
	if err := (&Env{}).ValidateFrontends(reg); err == nil || !strings.Contains(err.Error(), "DISCORD_BOT_TOKEN") {
		t.Errorf("missing token: error = %v", err)
	}
	if err := (&Env{DiscordBotToken: "t"}).ValidateFrontends(reg); err != nil {
		t.Errorf("token set: %v", err)
	}
	slackOnly := &Config{Clusters: []Cluster{{Frontends: ClusterFrontends{Slack: &SlackFrontend{SlashCommand: "/c"}}}}}
	if err := (&Env{SlackAppToken: "a", SlackBotToken: "b"}).ValidateFrontends(slackOnly); err != nil {
		t.Errorf("Discord token must not be required without Discord: %v", err)
	}
}

func TestHolder_DiscordLookupAndCommands(t *testing.T) {
	t.Parallel()

	a, b := discordCluster("a", "knock-b"), discordCluster("b", "knock-a")
	cfg := &Config{Controller: Controller{Namespace: "wk"}, Clusters: []Cluster{a, b}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h := NewHolder(cfg)
	if got := h.DiscordCommands(); len(got) != 2 || got[0] != "knock-a" || got[1] != "knock-b" {
		t.Errorf("DiscordCommands = %v, want sorted", got)
	}
	if cl, ok := h.ByDiscordCommand("knock-a"); !ok || cl.Env != "b" {
		t.Errorf("ByDiscordCommand = %+v, %v", cl, ok)
	}

	next := &Config{Controller: Controller{Namespace: "wk"}, Clusters: []Cluster{discordCluster("a", "renamed")}}
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
	h.Store(next)
	if _, ok := h.ByDiscordCommand("knock-a"); ok {
		t.Error("old command still resolves after Store")
	}
	if got := h.DiscordCommands(); len(got) != 1 || got[0] != "renamed" {
		t.Errorf("DiscordCommands after Store = %v", got)
	}
	if got := cfg.EnabledFrontends(); len(got) != 1 || got[0] != FrontendDiscord {
		t.Errorf("EnabledFrontends = %v", got)
	}
}
