// Package router picks which provider serves a request, in what fallback
// order, based on each combo's configured strategy and the current circuit
// breaker state of its providers.
package router

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/codexmark/kram-gateway/internal/breaker"
	"github.com/codexmark/kram-gateway/internal/config"
	"github.com/codexmark/kram-gateway/internal/provider"
)

// combo is a resolved, ordered fallback chain plus its round-robin cursor.
type combo struct {
	strategy  string
	providers []provider.Provider
	cursor    uint64 // atomic, used by round-robin
}

// Router resolves a combo ID to an ordered attempt list: which provider to
// try first, and which to fall back to if it fails or its breaker is open.
type Router struct {
	mu           sync.RWMutex
	combos       map[string]*combo
	defaultCombo string
	breakers     *breaker.Registry
}

// New builds a Router from config, wiring each combo to already-built
// provider adapters keyed by ID.
func New(cfg *config.Config, providers map[string]provider.Provider, breakers *breaker.Registry) (*Router, error) {
	combos := make(map[string]*combo, len(cfg.Combos))
	for _, cc := range cfg.Combos {
		ps := make([]provider.Provider, 0, len(cc.Providers))
		for _, pid := range cc.Providers {
			p, ok := providers[pid]
			if !ok {
				return nil, fmt.Errorf("combo %q references unbuilt provider %q", cc.ID, pid)
			}
			ps = append(ps, p)
		}
		combos[cc.ID] = &combo{strategy: cc.Strategy, providers: ps}
	}

	return &Router{
		combos:       combos,
		defaultCombo: cfg.DefaultCombo,
		breakers:     breakers,
	}, nil
}

// Resolve maps a client-supplied "model" value to a combo ID: an exact
// combo ID match wins, otherwise the configured default combo is used.
func (r *Router) Resolve(model string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if _, ok := r.combos[model]; ok {
		return model, nil
	}
	if r.defaultCombo != "" {
		if _, ok := r.combos[r.defaultCombo]; ok {
			return r.defaultCombo, nil
		}
	}
	return "", fmt.Errorf("no combo matches model %q and no default_combo is configured", model)
}

// ComboInfo is a read-only summary of a combo, for status/diagnostics
// surfaces that shouldn't reach into the router's internal state.
type ComboInfo struct {
	ID        string
	Strategy  string
	Providers []string
}

// Combos returns a summary of every configured combo, in the order the
// providers were declared — the same order the fallback chain follows.
func (r *Router) Combos() []ComboInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ComboInfo, 0, len(r.combos))
	for id, c := range r.combos {
		ids := make([]string, 0, len(c.providers))
		for _, p := range c.providers {
			ids = append(ids, p.ID())
		}
		out = append(out, ComboInfo{ID: id, Strategy: c.strategy, Providers: ids})
	}
	return out
}

// StrategyPrefixAffinity routes by a hash of the request's stable prefix
// instead of rotating per request.
//
// Round-robin has a cost that isn't obvious until you look for it: every
// major provider caches prompt prefixes server-side and bills cached
// input at a fraction of the normal rate, and that cache is per-provider.
// An agent turn resends a large, near-identical prefix (system prompt,
// tool definitions, conversation so far) on every tool round-trip — so
// rotating providers between those calls throws the cache away each time
// and pays full price for the same tokens, repeatedly. Pinning a given
// conversation to one provider keeps that cache warm.
//
// The fallback chain is unaffected: a rate-limited or failing provider
// still trips its breaker and drops out of the healthy set, at which
// point affinity simply resolves to a different provider. Round-robin
// spreads load *proactively*, which is the better choice when every
// provider in the chain is a tight free tier; affinity is better
// whenever cache economics dominate.
const StrategyPrefixAffinity = "prefix-affinity"

// Attempts returns the ordered list of providers to try for a combo.
// affinityKey identifies the caller's stable prompt prefix (empty is
// fine — it just degrades affinity to the declared order). Providers
// whose circuit breaker is open are skipped entirely.
func (r *Router) Attempts(comboID, affinityKey string) ([]provider.Provider, error) {
	r.mu.RLock()
	c, ok := r.combos[comboID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown combo %q", comboID)
	}

	healthy := make([]provider.Provider, 0, len(c.providers))
	for _, p := range c.providers {
		if r.breakers.Allow(p.ID()) {
			healthy = append(healthy, p)
		}
	}
	if len(healthy) == 0 {
		return nil, fmt.Errorf("combo %q: all providers are circuit-open", comboID)
	}

	if len(healthy) > 1 {
		switch c.strategy {
		case "round-robin":
			n := atomic.AddUint64(&c.cursor, 1)
			offset := int(n % uint64(len(healthy)))
			healthy = append(healthy[offset:], healthy[:offset]...)
		case StrategyPrefixAffinity:
			// Hashing over the *healthy* set, not the configured one, is
			// deliberate: it keeps the choice stable for as long as the
			// same providers are up, and reshuffles only when one drops
			// out or comes back — which is exactly when the cache was
			// going to be lost anyway.
			offset := int(hashString(affinityKey) % uint64(len(healthy)))
			healthy = append(healthy[offset:], healthy[:offset]...)
		}
	}

	return healthy, nil
}

// hashString is FNV-1a, inlined rather than pulled from hash/fnv because
// all this needs is a stable bucket index — not a hash.Hash, not
// collision resistance.
func hashString(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
