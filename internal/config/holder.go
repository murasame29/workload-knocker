package config

import "sync/atomic"

// Resolver looks clusters up by env or slash command. *Config (fixed) and *Holder (hot reloaded) implement it.
type Resolver interface {
	ByEnv(env string) (Cluster, bool)
	BySlashCommand(cmd string) (Cluster, bool)
}

// Holder serves the current validated Config and swaps it atomically on hot reload.
type Holder struct {
	cur atomic.Pointer[Config]
}

// NewHolder returns a Holder serving c.
func NewHolder(c *Config) *Holder {
	h := &Holder{}
	h.cur.Store(c)
	return h
}

// Load returns the current Config.
func (h *Holder) Load() *Config { return h.cur.Load() }

// Store replaces the current Config; c must already be validated.
func (h *Holder) Store(c *Config) { h.cur.Store(c) }

// ByEnv implements Resolver.
func (h *Holder) ByEnv(env string) (Cluster, bool) { return h.cur.Load().ByEnv(env) }

// BySlashCommand implements Resolver.
func (h *Holder) BySlashCommand(cmd string) (Cluster, bool) {
	return h.cur.Load().BySlashCommand(cmd)
}
