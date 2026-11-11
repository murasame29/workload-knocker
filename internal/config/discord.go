package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
	_ "time/tzdata" // the runtime image may not ship a time zone database
)

const discordCommandMaxLen = 32

var (
	discordCommandPattern = regexp.MustCompile(`^[-_a-z0-9]+$`)
	discordChannelPattern = regexp.MustCompile(`^[0-9]{17,20}$`)
	slackChannelPattern   = regexp.MustCompile(`^[CG][A-Z0-9]{2,}$`)
)

// validateDiscordChannel accepts an empty value (no notify channel) or a numeric snowflake.
func validateDiscordChannel(id string) error {
	if id != "" && !discordChannelPattern.MatchString(id) {
		return fmt.Errorf("%q must be a numeric channel ID (17-20 digits)", id)
	}
	return nil
}

// validateSlackChannel accepts an empty value (no notify channel) or a channel ID such as C0123ABCD.
func validateSlackChannel(id string) error {
	if id != "" && !slackChannelPattern.MatchString(id) {
		return fmt.Errorf("%q must be a channel ID such as C0123ABCD (a channel name is not accepted)", id)
	}
	return nil
}

// DiscordFrontend holds Discord settings. Command is the slash command name, unique in the registry.
type DiscordFrontend struct {
	Command string `yaml:"command"`
	// NotifyChannel is an optional channel ID (snowflake) that receives public start, ready and stop messages.
	NotifyChannel string `yaml:"notifyChannel,omitempty"`
	// TimeZone is the IANA time zone in which reservation times are entered; empty means UTC.
	TimeZone string `yaml:"timeZone,omitempty"`
}

// Location returns the time zone of reservation times entered in Discord; UTC when unset or invalid
// (Validate rejects invalid names).
func (d *DiscordFrontend) Location() *time.Location {
	if d == nil || d.TimeZone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(d.TimeZone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func validateTimeZone(name string) error {
	if name == "" {
		return nil
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("unknown time zone %q (use an IANA name such as Asia/Tokyo)", name)
	}
	return nil
}

// ByDiscordCommand resolves the cluster of a Discord slash command name.
func (c *Config) ByDiscordCommand(cmd string) (Cluster, bool) {
	cl, ok := c.byDiscordCmd[cmd]
	return cl, ok
}

// DiscordCommands returns the configured Discord command names, sorted.
func (c *Config) DiscordCommands() []string {
	cmds := make([]string, 0, len(c.byDiscordCmd))
	for name := range c.byDiscordCmd {
		cmds = append(cmds, name)
	}
	slices.Sort(cmds)
	return cmds
}

// validateDiscordCommand enforces Discord's chat input command name rules (no leading slash).
func validateDiscordCommand(cmd string) error {
	switch {
	case cmd == "":
		return errors.New("is required")
	case len(cmd) > discordCommandMaxLen:
		return fmt.Errorf("must be at most %d characters", discordCommandMaxLen)
	case !discordCommandPattern.MatchString(cmd):
		return fmt.Errorf("%q must be lowercase letters, digits, '-' or '_' without a leading slash", cmd)
	}
	return nil
}
