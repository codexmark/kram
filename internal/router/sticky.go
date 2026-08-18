package router

import (
	"sync"
	"time"
)

// stickyMaxEntries bounds the sticky pin table so an abandoned session's
// key doesn't grow it forever — a stuck pin costs a little memory, never
// correctness, and isn't worth a background sweeper goroutine to clean up
// (see DECISIONS.md).
const stickyMaxEntries = 256

type stickyEntry struct {
	provider string
	lastUsed time.Time
}

// stickyStore pins a run — identified by RouteContext.RunKey, not the
// prompt-prefix AffinityKey prefix-affinity routing uses (see RunKey's
// doc comment for why those two had to stop being the same thing) — to
// its winning provider across tool round-trips. See DECISIONS.md, "Smart
// Sticky":
// trading provider diversity for prompt-cache economics and predictable
// behavior within a single agent run, which is exactly what an agent
// loop's tool round-trips want.
type stickyStore struct {
	mu      sync.Mutex
	entries map[string]stickyEntry
}

func newStickyStore() *stickyStore {
	return &stickyStore{entries: make(map[string]stickyEntry)}
}

// get returns the pinned provider ID for key, or "" if there's no pin (a
// fresh run, or one that was never pinned yet).
func (s *stickyStore) get(key string) string {
	if key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return ""
	}
	e.lastUsed = time.Now()
	s.entries[key] = e
	return e.provider
}

// set pins key to providerID — called after a request actually completes
// successfully, so "the sticky winner" always means "who last actually
// served this run," never a guess.
func (s *stickyStore) set(key, providerID string) {
	if key == "" || providerID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[key]; !exists && len(s.entries) >= stickyMaxEntries {
		s.evictOldestLocked()
	}
	s.entries[key] = stickyEntry{provider: providerID, lastUsed: time.Now()}
}

func (s *stickyStore) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, e := range s.entries {
		if first || e.lastUsed.Before(oldestTime) {
			oldestKey, oldestTime, first = k, e.lastUsed, false
		}
	}
	if oldestKey != "" {
		delete(s.entries, oldestKey)
	}
}
