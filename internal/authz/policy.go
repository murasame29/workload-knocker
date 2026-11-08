// Package authz decides which subject (chat user or API caller) may start or stop which workload.
package authz

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the policy file used when AUTHZ_CONFIG_PATH is unset.
const DefaultPath = "/etc/workload-knocker/authz.yaml"

// Platform identifies where a Subject comes from.
type Platform string

const (
	PlatformSlack   Platform = "slack"
	PlatformDiscord Platform = "discord"
	PlatformAPI     Platform = "api"
)

// Action is a workload operation a policy rule can grant.
type Action string

const (
	ActionStart Action = "start"
	ActionStop  Action = "stop"
)

// Subject is who asks for an operation. Groups are Slack usergroup IDs or Discord role IDs.
type Subject struct {
	Platform Platform
	User     string
	Groups   []string
}

// APISubject is the single subject of every authenticated REST API call.
var APISubject = Subject{Platform: PlatformAPI}

// Authorizer answers whether subject may perform action on env/namespace/workload.
type Authorizer interface {
	Allowed(s Subject, env, namespace, workload string, a Action) bool
}

var (
	slackUserRe  = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)
	slackGroupRe = regexp.MustCompile(`^S[A-Z0-9]{2,}$`)
	snowflakeRe  = regexp.MustCompile(`^[0-9]{17,20}$`)
)

type rawPolicy struct {
	Version int       `yaml:"version"`
	Default string    `yaml:"default"`
	Rules   []rawRule `yaml:"rules"`
}

type rawRule struct {
	Env        string      `yaml:"env"`
	Namespaces []string    `yaml:"namespaces"`
	Workloads  []string    `yaml:"workloads"`
	Actions    []string    `yaml:"actions"`
	Subjects   rawSubjects `yaml:"subjects"`
}

type rawSubjects struct {
	Slack   rawChat `yaml:"slack"`
	Discord struct {
		Users []string `yaml:"users"`
		Roles []string `yaml:"roles"`
	} `yaml:"discord"`
	API bool `yaml:"api"`
}

type rawChat struct {
	Users  []string `yaml:"users"`
	Groups []string `yaml:"groups"`
}

type rule struct {
	env                        string
	namespaces, workloads      []string
	start, stop                bool
	slackUsers, slackGroups    []string
	discordUsers, discordRoles []string
	api                        bool
}

// Policy is an immutable, validated authorization policy.
type Policy struct {
	defaultAllow bool
	rules        []rule
}

// Parse validates YAML or JSON bytes into a Policy.
func Parse(data []byte) (*Policy, error) {
	var raw rawPolicy
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("authz policy is empty")
		}
		return nil, fmt.Errorf("parse authz policy: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("authz policy must contain a single document")
	}

	if raw.Version != 1 {
		return nil, fmt.Errorf("authz policy version must be 1 (got %d)", raw.Version)
	}
	p := &Policy{}
	switch raw.Default {
	case "", "deny":
	case "allow":
		p.defaultAllow = true
	default:
		return nil, fmt.Errorf("authz policy default must be deny or allow (got %q)", raw.Default)
	}
	for i, r := range raw.Rules {
		cr, err := compileRule(r)
		if err != nil {
			return nil, fmt.Errorf("authz rules[%d]: %w", i, err)
		}
		p.rules = append(p.rules, cr)
	}
	return p, nil
}

// Load reads and validates the policy file at path.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authz policy %s: %w", path, err)
	}
	return ParseFile(path, data)
}

// ParseFile is Parse with the file path added to errors.
func ParseFile(path string, data []byte) (*Policy, error) {
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// LoadConfigured resolves AUTHZ_CONFIG_PATH. It returns a nil Policy (authorization disabled)
// only when envPath is empty and the default file does not exist.
func LoadConfigured(envPath string) (*Policy, string, error) {
	if envPath != "" {
		p, err := Load(envPath)
		return p, envPath, err
	}
	if _, err := os.Stat(DefaultPath); errors.Is(err, os.ErrNotExist) {
		return nil, DefaultPath, nil
	}
	p, err := Load(DefaultPath)
	return p, DefaultPath, err
}

func compileRule(r rawRule) (rule, error) {
	var out rule
	if r.Env == "" {
		return out, errors.New("env is required (use \"*\" for every env)")
	}
	if err := validGlobs("env", []string{r.Env}); err != nil {
		return out, err
	}
	if len(r.Namespaces) == 0 {
		return out, errors.New("namespaces must list at least one glob")
	}
	if err := validGlobs("namespaces", r.Namespaces); err != nil {
		return out, err
	}
	out.env, out.namespaces = r.Env, slices.Clone(r.Namespaces)

	out.workloads = []string{"*"}
	if len(r.Workloads) > 0 {
		if err := validGlobs("workloads", r.Workloads); err != nil {
			return out, err
		}
		out.workloads = slices.Clone(r.Workloads)
	}

	if len(r.Actions) == 0 {
		out.start, out.stop = true, true
	}
	for _, a := range r.Actions {
		switch Action(a) {
		case ActionStart:
			out.start = true
		case ActionStop:
			out.stop = true
		default:
			return out, fmt.Errorf("unknown action %q (allowed: start, stop)", a)
		}
	}

	s := r.Subjects
	var err error
	if out.slackUsers, err = validIDs("subjects.slack.users", s.Slack.Users, slackUserRe); err != nil {
		return out, err
	}
	if out.slackGroups, err = validIDs("subjects.slack.groups", s.Slack.Groups, slackGroupRe); err != nil {
		return out, err
	}
	if out.discordUsers, err = validIDs("subjects.discord.users", s.Discord.Users, snowflakeRe); err != nil {
		return out, err
	}
	if out.discordRoles, err = validIDs("subjects.discord.roles", s.Discord.Roles, snowflakeRe); err != nil {
		return out, err
	}
	out.api = s.API
	if len(out.slackUsers)+len(out.slackGroups)+len(out.discordUsers)+len(out.discordRoles) == 0 && !out.api {
		return out, errors.New("subjects must list at least one user, group, role or api: true")
	}
	return out, nil
}

func validGlobs(field string, globs []string) error {
	for _, g := range globs {
		if g == "" {
			return fmt.Errorf("%s contains an empty pattern", field)
		}
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("%s: invalid glob %q: %w", field, g, err)
		}
	}
	return nil
}

func validIDs(field string, ids []string, re *regexp.Regexp) ([]string, error) {
	for _, id := range ids {
		if !re.MatchString(id) {
			return nil, fmt.Errorf("%s: %q is not a valid ID (expected %s)", field, id, re)
		}
	}
	return slices.Clone(ids), nil
}

// Allowed reports whether s may perform a on env/namespace/workload.
// Any matching rule that lists the subject allows; otherwise the policy default applies.
func (p *Policy) Allowed(s Subject, env, namespace, workload string, a Action) bool {
	for i := range p.rules {
		r := &p.rules[i]
		if !r.grants(a) || !globMatch(r.env, env) || !anyGlob(r.namespaces, namespace) || !anyGlob(r.workloads, workload) {
			continue
		}
		if r.listsSubject(s) {
			return true
		}
	}
	return p.defaultAllow
}

// Groups returns the group or role IDs the policy references for platform, sorted and unique.
func (p *Policy) Groups(platform Platform) []string {
	var out []string
	for i := range p.rules {
		switch platform {
		case PlatformSlack:
			out = append(out, p.rules[i].slackGroups...)
		case PlatformDiscord:
			out = append(out, p.rules[i].discordRoles...)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Rules returns the number of rules.
func (p *Policy) Rules() int { return len(p.rules) }

// DefaultAllow reports whether unmatched requests are allowed.
func (p *Policy) DefaultAllow() bool { return p.defaultAllow }

func (r *rule) grants(a Action) bool {
	switch a {
	case ActionStart:
		return r.start
	case ActionStop:
		return r.stop
	}
	return false
}

func (r *rule) listsSubject(s Subject) bool {
	switch s.Platform {
	case PlatformSlack:
		return s.User != "" && slices.Contains(r.slackUsers, s.User) || anyIn(s.Groups, r.slackGroups)
	case PlatformDiscord:
		return s.User != "" && slices.Contains(r.discordUsers, s.User) || anyIn(s.Groups, r.discordRoles)
	case PlatformAPI:
		return r.api
	}
	return false
}

func anyIn(have, want []string) bool {
	for _, h := range have {
		if h != "" && slices.Contains(want, h) {
			return true
		}
	}
	return false
}

func anyGlob(globs []string, name string) bool {
	return slices.ContainsFunc(globs, func(g string) bool { return globMatch(g, name) })
}

func globMatch(glob, name string) bool {
	ok, err := path.Match(glob, name)
	return err == nil && ok
}

// Holder swaps the active policy atomically; a nil policy disables authorization (allow all).
type Holder struct {
	cur atomic.Pointer[Policy]
}

// NewHolder returns a Holder serving p (nil = authorization disabled).
func NewHolder(p *Policy) *Holder {
	h := &Holder{}
	h.cur.Store(p)
	return h
}

// Store replaces the active policy.
func (h *Holder) Store(p *Policy) { h.cur.Store(p) }

// Policy returns the active policy, or nil when authorization is disabled.
func (h *Holder) Policy() *Policy { return h.cur.Load() }

// Allowed implements Authorizer.
func (h *Holder) Allowed(s Subject, env, namespace, workload string, a Action) bool {
	p := h.cur.Load()
	return p == nil || p.Allowed(s, env, namespace, workload, a)
}

// Groups returns the group or role IDs referenced by the active policy for platform.
func (h *Holder) Groups(platform Platform) []string {
	if p := h.cur.Load(); p != nil {
		return p.Groups(platform)
	}
	return nil
}

// PathOrDefault returns p, or DefaultPath when p is empty.
func PathOrDefault(p string) string {
	if p == "" {
		return DefaultPath
	}
	return p
}
