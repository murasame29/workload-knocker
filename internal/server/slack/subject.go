package slackbot

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/authz"
)

const (
	// groupCacheTTL bounds how stale a usergroup membership may be.
	groupCacheTTL = 60 * time.Second
	// resolveTimeout keeps subject resolution inside Slack's 3s trigger_id window.
	resolveTimeout = time.Second
)

// membersAPI is the slack.Client method the group cache needs (scope usergroups:read).
type membersAPI interface {
	GetUserGroupMembersContext(ctx context.Context, userGroup string, options ...slack.GetUserGroupMembersOption) ([]string, error)
}

// groupCache caches usergroup members for a short TTL; only groups the policy references are fetched.
type groupCache struct {
	api membersAPI
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[string]groupEntry
}

type groupEntry struct {
	members map[string]struct{}
	expires time.Time
}

func newGroupCache(api membersAPI, ttl time.Duration) *groupCache {
	return &groupCache{api: api, ttl: ttl, now: time.Now, entries: map[string]groupEntry{}}
}

// isMember reports whether user belongs to the usergroup; errors are never cached.
func (c *groupCache) isMember(ctx context.Context, group, user string) (bool, error) {
	c.mu.Lock()
	e, ok := c.entries[group]
	c.mu.Unlock()
	if ok && c.now().Before(e.expires) {
		_, in := e.members[user]
		return in, nil
	}

	ids, err := c.api.GetUserGroupMembersContext(ctx, group)
	if err != nil {
		return false, err
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	c.mu.Lock()
	now := c.now()
	for g, old := range c.entries {
		if !now.Before(old.expires) {
			delete(c.entries, g)
		}
	}
	c.entries[group] = groupEntry{members: set, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	_, in := set[user]
	return in, nil
}

// subject builds the authz subject of a Slack user: the user ID plus the policy-referenced usergroups
// the user belongs to. A group that cannot be resolved is left out (never fail open).
func (b *Bot) subject(userID string) authz.Subject {
	sub := authz.Subject{Platform: authz.PlatformSlack, User: userID}
	candidates := b.authz.Groups(authz.PlatformSlack)
	if len(candidates) == 0 {
		return sub
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	in := make([]bool, len(candidates))
	var wg sync.WaitGroup
	for i, g := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := b.groups.isMember(ctx, g, userID)
			if err != nil {
				slog.Warn("usergroup lookup failed, matching the user ID only for this group", slog.String("usergroup", g), slog.String("error.message", err.Error()))
			}
			in[i] = ok
		}()
	}
	wg.Wait()
	for i, g := range candidates {
		if in[i] {
			sub.Groups = append(sub.Groups, g)
		}
	}
	return sub
}
