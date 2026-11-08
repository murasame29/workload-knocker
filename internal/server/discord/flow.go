package discordbot

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/murasame29/workload-knocker/internal/frontend"
)

const (
	// flowTTL is how long an untouched interaction flow stays usable.
	flowTTL = 15 * time.Minute
	// maxFlows bounds memory when many flows are opened and abandoned inside the TTL.
	maxFlows = 1000
)

// flow is the per-invocation state behind one ephemeral message. The custom_id carries only
// the token; selection state lives here because Discord select menus cap value length at 100
// and a plan can hold many workload names.
type flow struct {
	userID    string
	env       string
	namespace string
	// nsPage / page are the visible pages of the namespace and workload menus.
	nsPage int
	page   int
	// entries is the snapshot taken at namespace selection; select option values are indexes into it.
	entries []frontend.Entry
	// selected holds the representative names (Entry.Value) of entries to keep running.
	selected map[string]bool
	toStart  []string
	toStop   []string
	// requestID ties the preview and the confirm of one operation in the audit log.
	requestID string
	expires   time.Time
}

// flowStore is an in-memory TTL store of flows keyed by a random token. A restart drops all
// flows; users then rerun the command (single replica, short-lived interactions).
type flowStore struct {
	mu    sync.Mutex
	flows map[string]*flow
	ttl   time.Duration
	now   func() time.Time
}

func newFlowStore(ttl time.Duration) *flowStore {
	return &flowStore{flows: make(map[string]*flow), ttl: ttl, now: time.Now}
}

// create stores f and returns its token.
func (s *flowStore) create(f *flow) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if len(s.flows) >= maxFlows {
		s.evictOldestLocked()
	}
	f.expires = s.now().Add(s.ttl)
	s.flows[token] = f
	return token, nil
}

// with runs fn on the live flow under the store lock and extends its TTL.
// It returns false when the token is unknown or expired.
func (s *flowStore) with(token string, fn func(*flow)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[token]
	if !ok || !f.expires.After(s.now()) {
		delete(s.flows, token)
		return false
	}
	f.expires = s.now().Add(s.ttl)
	fn(f)
	return true
}

// take removes and returns the flow so a confirm cannot run twice.
func (s *flowStore) take(token string) (*flow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[token]
	delete(s.flows, token)
	if !ok || !f.expires.After(s.now()) {
		return nil, false
	}
	return f, true
}

func (s *flowStore) sweepLocked() {
	now := s.now()
	for k, f := range s.flows {
		if !f.expires.After(now) {
			delete(s.flows, k)
		}
	}
}

func (s *flowStore) evictOldestLocked() {
	var oldest string
	var at time.Time
	for k, f := range s.flows {
		if oldest == "" || f.expires.Before(at) {
			oldest, at = k, f.expires
		}
	}
	delete(s.flows, oldest)
}
