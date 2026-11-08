package authz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const samplePolicy = `
version: 1
default: deny
rules:
  - env: stg
    namespaces: ["team-*"]
    workloads: ["checkout-*", "web"]
    actions: [start]
    subjects:
      slack:
        users: [U111AAAA]
        groups: [S222BBBB]
  - env: "*"
    namespaces: ["sandbox"]
    subjects:
      slack: {users: [U333CCCC]}
      discord: {users: ["123456789012345678"], roles: ["223456789012345678"]}
      api: true
`

func mustParse(t *testing.T, doc string) *Policy {
	t.Helper()
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestPolicy_Allowed(t *testing.T) {
	t.Parallel()

	p := mustParse(t, samplePolicy)
	slackUser := Subject{Platform: PlatformSlack, User: "U111AAAA"}
	slackMember := Subject{Platform: PlatformSlack, User: "UOTHER99", Groups: []string{"S222BBBB"}}
	stranger := Subject{Platform: PlatformSlack, User: "UNKNOWN9"}

	cases := []struct {
		name     string
		s        Subject
		env, ns  string
		workload string
		action   Action
		want     bool
	}{
		{"user matches glob rule", slackUser, "stg", "team-a", "checkout-api", ActionStart, true},
		{"usergroup member matches", slackMember, "stg", "team-b", "web", ActionStart, true},
		{"action not granted", slackUser, "stg", "team-a", "web", ActionStop, false},
		{"workload glob miss", slackUser, "stg", "team-a", "billing", ActionStart, false},
		{"namespace glob miss", slackUser, "stg", "other", "web", ActionStart, false},
		{"env mismatch", slackUser, "dev", "team-a", "web", ActionStart, false},
		{"unlisted subject denied by default", stranger, "stg", "team-a", "web", ActionStart, false},
		{"wildcard env, default actions grant stop", Subject{Platform: PlatformSlack, User: "U333CCCC"}, "dev", "sandbox", "anything", ActionStop, true},
		{"api subject", APISubject, "prod", "sandbox", "x", ActionStop, true},
		{"api not listed on first rule", APISubject, "stg", "team-a", "web", ActionStart, false},
		{"discord user", Subject{Platform: PlatformDiscord, User: "123456789012345678"}, "stg", "sandbox", "x", ActionStart, true},
		{"discord role", Subject{Platform: PlatformDiscord, User: "999", Groups: []string{"223456789012345678"}}, "stg", "sandbox", "x", ActionStart, true},
		{"slack ID is not a discord user", Subject{Platform: PlatformDiscord, User: "U333CCCC"}, "stg", "sandbox", "x", ActionStart, false},
		{"slack group ID is ignored for discord", Subject{Platform: PlatformDiscord, Groups: []string{"S222BBBB"}}, "stg", "team-a", "web", ActionStart, false},
		{"empty user never matches", Subject{Platform: PlatformSlack}, "stg", "team-a", "web", ActionStart, false},
		{"unknown platform", Subject{Platform: "teams", User: "U111AAAA"}, "stg", "team-a", "web", ActionStart, false},
		{"unknown action", slackUser, "stg", "team-a", "web", Action("restart"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := p.Allowed(tc.s, tc.env, tc.ns, tc.workload, tc.action); got != tc.want {
				t.Errorf("Allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPolicy_DefaultAllowAndDeny(t *testing.T) {
	t.Parallel()

	s := Subject{Platform: PlatformSlack, User: "UNKNOWN9"}
	if mustParse(t, "version: 1\n").Allowed(s, "stg", "ns", "w", ActionStart) {
		t.Error("omitted default must deny")
	}
	if !mustParse(t, "version: 1\ndefault: allow\n").Allowed(s, "stg", "ns", "w", ActionStop) {
		t.Error("default allow must allow when no rule matches")
	}
}

func TestPolicy_GlobEdgeCases(t *testing.T) {
	t.Parallel()

	p := mustParse(t, `
version: 1
rules:
  - env: "stg*"
    namespaces: ["a?c", "[xy]z", "exact"]
    workloads: ["*-worker", "w[0-9]"]
    subjects: {api: true}
`)
	cases := []struct {
		env, ns, w string
		want       bool
	}{
		{"stg", "abc", "foo-worker", true},
		{"stg-us", "xz", "w1", true},
		{"stg", "exact", "-worker", true},
		{"stg", "ac", "foo-worker", false},
		{"stg", "abc", "foo-worker/extra", false},
		{"stg", "Exact", "foo-worker", false},
		{"dev", "abc", "foo-worker", false},
		{"stg", "abc", "w10", false},
		{"stg", "", "foo-worker", false},
	}
	for _, tc := range cases {
		if got := p.Allowed(APISubject, tc.env, tc.ns, tc.w, ActionStart); got != tc.want {
			t.Errorf("env=%q ns=%q w=%q: got %v, want %v", tc.env, tc.ns, tc.w, got, tc.want)
		}
	}
}

func TestParse_JSON(t *testing.T) {
	t.Parallel()

	p := mustParse(t, `{"version":1,"default":"deny","rules":[{"env":"*","namespaces":["*"],"subjects":{"api":true}}]}`)
	if !p.Allowed(APISubject, "stg", "ns", "w", ActionStop) {
		t.Error("JSON policy rule did not apply")
	}
}

func TestParse_Errors(t *testing.T) {
	t.Parallel()

	rule := func(body string) string {
		return "version: 1\nrules:\n  - " + body + "\n"
	}
	cases := []struct {
		name, doc, want string
	}{
		{"empty", "", "empty"},
		{"not yaml", "version: [", "parse"},
		{"missing version", "default: deny\n", "version must be 1"},
		{"wrong version", "version: 2\n", "version must be 1"},
		{"bad default", "version: 1\ndefault: maybe\n", "default must be"},
		{"unknown top-level key", "version: 1\nrulez: []\n", "rulez"},
		{"unknown rule key", rule("{env: stg, namespaces: [a], subjects: {api: true}, deny: true}"), "deny"},
		{"two documents", "version: 1\n---\nversion: 1\n", "single document"},
		{"missing env", rule("{namespaces: [a], subjects: {api: true}}"), "env is required"},
		{"missing namespaces", rule("{env: stg, subjects: {api: true}}"), "namespaces"},
		{"bad glob", rule("{env: stg, namespaces: ['a['], subjects: {api: true}}"), "invalid glob"},
		{"bad workload glob", rule("{env: stg, namespaces: [a], workloads: ['['], subjects: {api: true}}"), "invalid glob"},
		{"empty glob", rule("{env: stg, namespaces: [''], subjects: {api: true}}"), "empty pattern"},
		{"unknown action", rule("{env: stg, namespaces: [a], actions: [restart], subjects: {api: true}}"), "unknown action"},
		{"no subjects", rule("{env: stg, namespaces: [a]}"), "at least one"},
		{"api false is no subject", rule("{env: stg, namespaces: [a], subjects: {api: false}}"), "at least one"},
		{"group ID under users", rule("{env: stg, namespaces: [a], subjects: {slack: {users: [S222BBBB]}}}"), "not a valid ID"},
		{"user ID under groups", rule("{env: stg, namespaces: [a], subjects: {slack: {groups: [U111AAAA]}}}"), "not a valid ID"},
		{"bad snowflake", rule("{env: stg, namespaces: [a], subjects: {discord: {users: ['12ab']}}}"), "not a valid ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestPolicy_Groups(t *testing.T) {
	t.Parallel()

	p := mustParse(t, samplePolicy+`
  - env: dev
    namespaces: ["*"]
    subjects: {slack: {groups: [S222BBBB, S444DDDD]}}
`)
	if got := strings.Join(p.Groups(PlatformSlack), ","); got != "S222BBBB,S444DDDD" {
		t.Errorf("slack groups = %q", got)
	}
	if got := strings.Join(p.Groups(PlatformDiscord), ","); got != "223456789012345678" {
		t.Errorf("discord roles = %q", got)
	}
	if p.Groups(PlatformAPI) != nil {
		t.Error("api has no groups")
	}
}

func TestHolder(t *testing.T) {
	t.Parallel()

	s := Subject{Platform: PlatformSlack, User: "UNKNOWN9"}
	h := NewHolder(nil)
	if !h.Allowed(s, "stg", "ns", "w", ActionStop) {
		t.Error("nil policy must allow all")
	}
	if h.Groups(PlatformSlack) != nil {
		t.Error("nil policy has no groups")
	}
	h.Store(mustParse(t, "version: 1\n"))
	if h.Allowed(s, "stg", "ns", "w", ActionStop) {
		t.Error("deny-all policy must deny")
	}
	if h.Policy() == nil {
		t.Error("Policy() must return the stored policy")
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	good := filepath.Join(dir, "authz.yaml")
	if err := os.WriteFile(good, []byte(samplePolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(good); err != nil {
		t.Errorf("Load(good): %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("Load(missing) must fail")
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("Load(bad) = %v, want error naming the file", err)
	}
}

func TestLoadConfigured_ExplicitPathMustExist(t *testing.T) {
	t.Parallel()

	if _, _, err := LoadConfigured(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("explicit missing path must fail startup")
	}
}
