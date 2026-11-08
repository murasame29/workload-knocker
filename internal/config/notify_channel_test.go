package config

import (
	"strings"
	"testing"
)

func TestValidate_NotifyChannel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, yaml, errSubstr string
	}{
		{"none", "discord: {command: knock}\n      slack: {slashCommand: /knock}", ""},
		{"discord snowflake", "discord: {command: knock, notifyChannel: \"123456789012345678\"}", ""},
		{"discord name", "discord: {command: knock, notifyChannel: general}", "frontends.discord.notifyChannel"},
		{"discord too short", "discord: {command: knock, notifyChannel: \"12345\"}", "numeric channel ID"},
		{"discord mention", "discord: {command: knock, notifyChannel: \"<#123456789012345678>\"}", "numeric channel ID"},
		{"slack id", "slack: {slashCommand: /knock, notifyChannel: C0123ABCD}", ""},
		{"slack private id", "slack: {slashCommand: /knock, notifyChannel: G0123ABCD}", ""},
		{"slack name", "slack: {slashCommand: /knock, notifyChannel: \"#general\"}", "frontends.slack.notifyChannel"},
		{"slack lowercase", "slack: {slashCommand: /knock, notifyChannel: c0123abcd}", "channel ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := "controller: {namespace: wk}\nclusters:\n  - env: stg\n    name: stg\n    provider: {type: eks, eks: {region: ap-northeast-1}}\n    frontends:\n      " + tc.yaml + "\n"
			_, err := ParseValidated("test.yaml", []byte(data))
			switch {
			case tc.errSubstr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.errSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.errSubstr)):
				t.Fatalf("error = %v, want substring %q", err, tc.errSubstr)
			}
		})
	}
}

func TestCluster_NotifyChannel(t *testing.T) {
	t.Parallel()

	cl := Cluster{Frontends: ClusterFrontends{
		Discord: &DiscordFrontend{Command: "k", NotifyChannel: "123456789012345678"},
		Slack:   &SlackFrontend{SlashCommand: "/k"},
	}}
	if got := cl.NotifyChannel(FrontendDiscord); got != "123456789012345678" {
		t.Errorf("discord = %q", got)
	}
	if got := cl.NotifyChannel(FrontendSlack); got != "" {
		t.Errorf("slack = %q, want empty", got)
	}
	if got := (Cluster{}).NotifyChannel(FrontendDiscord); got != "" {
		t.Errorf("unconfigured frontend = %q", got)
	}
}
