package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

const discordCommandMaxLen = 32

var discordCommandPattern = regexp.MustCompile(`^[-_a-z0-9]+$`)

// DiscordFrontend holds Discord settings. Command is the slash command name, unique in the registry.
type DiscordFrontend struct {
	Command string `yaml:"command"`
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
