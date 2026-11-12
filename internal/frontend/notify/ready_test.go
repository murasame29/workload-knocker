package notify

import (
	"strings"
	"testing"
	"time"
)

func TestRenderReady(t *testing.T) {
	t.Parallel()

	d := ReadyData{Name: "terraria", Namespace: "games", Cluster: "stg", StartedBy: "alice", StopAt: "22:00 JST"}
	cases := []struct {
		name, tmpl, want string
	}{
		{"all fields", "{{.Name}}/{{.Namespace}}/{{.Cluster}} by {{.StartedBy}} until {{.StopAt}}", "terraria/games/stg by alice until 22:00 JST"},
		{"conditional stop time", "Connect{{if .StopAt}} (until {{.StopAt}}){{end}}", "Connect (until 22:00 JST)"},
		{"trimmed", "  hello \n", "hello"},
		{"empty template", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := RenderReady(tc.tmpl, d)
			if err != nil || got != tc.want {
				t.Errorf("RenderReady = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestRenderReady_EmptyStopAtAndCap(t *testing.T) {
	t.Parallel()

	got, err := RenderReady("Connect{{if .StopAt}} until {{.StopAt}}{{end}}", ReadyData{})
	if err != nil || got != "Connect" {
		t.Errorf("empty StopAt: %q, %v", got, err)
	}
	long, err := RenderReady(strings.Repeat("あ", 900), ReadyData{})
	if err != nil || len([]rune(long)) != maxReadyRunes || !strings.HasSuffix(long, "…") {
		t.Errorf("cap: %d runes, %v", len([]rune(long)), err)
	}
}

func TestRenderReady_Errors(t *testing.T) {
	t.Parallel()

	for _, tmpl := range []string{"{{.Name", "{{.Secret}}", "{{template \"x\"}}", "{{index .Name 99}}"} {
		if _, err := RenderReady(tmpl, ReadyData{Name: "a"}); err == nil {
			t.Errorf("RenderReady(%q): want error", tmpl)
		}
		if err := ValidateReady(tmpl); err == nil {
			t.Errorf("ValidateReady(%q): want error", tmpl)
		}
	}
}

func TestValidateReady_Valid(t *testing.T) {
	t.Parallel()

	for _, tmpl := range []string{"Connect to {{.Name}}.example.com:7777", "plain text", "{{if .StopAt}}until {{.StopAt}}{{end}}"} {
		if err := ValidateReady(tmpl); err != nil {
			t.Errorf("ValidateReady(%q) = %v", tmpl, err)
		}
	}
}

func TestFormatStopAt(t *testing.T) {
	t.Parallel()

	if FormatStopAt(time.Time{}) != "" {
		t.Error("zero time must format as empty")
	}
	jst := time.FixedZone("JST", 9*3600)
	if got := FormatStopAt(time.Date(2026, 1, 1, 22, 0, 0, 0, jst)); got != "22:00 JST" {
		t.Errorf("FormatStopAt = %q", got)
	}
}
