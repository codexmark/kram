// Package router decides which provider serves a request, in what order,
// based on each combo's configured strategy — and, once a response comes
// back, whether it was actually good enough to accept. Three concepts are
// kept deliberately separate (see DECISIONS.md, "Combos v2"):
//
//  1. Routing strategy (this package's Strategy interface): decides WHO
//     to try and in WHAT ORDER — a full ranking, not just a winner.
//  2. Attempt execution (internal/server/chat.go): actually calls each
//     candidate in ranked order until one is accepted or the chain is
//     exhausted.
//  3. Response gate (gate.go, stream.go): decides whether a technically-
//     successful response is good enough to end the fallback chain.
package router

import (
	"fmt"
	"sync"

	"github.com/codexmark/kram/internal/breaker"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/provider"
	"github.com/codexmark/kram/internal/telemetry"
)

// combo is a resolved combo: its provider pool, its strategy instance
// (built once at Router construction and reused across every request —
// this is what lets round-robin's cursor and the weighted strategy's
// sticky/LKGP state persist across calls), and its response gate config.
type combo struct {
	id           string
	strategyName string
	strategy     Strategy
	strategyOpts strategyOptions
	providers    []provider.Provider
	response     config.ResponseGateConfig
}

// Router resolves a combo ID to a ranked candidate list for one request.
type Router struct {
	mu           sync.RWMutex
	combos       map[string]*combo
	defaultCombo string
	breakers     *breaker.Registry
	telemetry    *telemetry.Registry
	// qualityHints is provider ID -> operator-configured QualityHint,
	// resolved once at construction — see config.ProviderConfig.QualityHint.
	qualityHints map[string]float64
}

// New builds a Router from config, wiring each combo to already-built
// provider adapters keyed by ID and constructing its configured Strategy.
func New(cfg *config.Config, providers map[string]provider.Provider, breakers *breaker.Registry, tel *telemetry.Registry) (*Router, error) {
	qualityHints := make(map[string]float64, len(cfg.Providers))
	for _, pc := range cfg.Providers {
		if pc.QualityHint > 0 {
			qualityHints[pc.ID] = pc.QualityHint
		}
	}

	combos := make(map[string]*combo, len(cfg.Combos))
	for _, cc := range cfg.Combos {
		if !validStrategyName(cc.Strategy) {
			return nil, unknownStrategyError(cc.ID, cc.Strategy)
		}
		ps := make([]provider.Provider, 0, len(cc.Providers))
		for _, pid := range cc.Providers {
			p, ok := providers[pid]
			if !ok {
				return nil, fmt.Errorf("combo %q references unbuilt provider %q", cc.ID, pid)
			}
			ps = append(ps, p)
		}
		opts := resolveStrategyOptions(cc.StrategyOptions)
		combos[cc.ID] = &combo{
			id: cc.ID, strategyName: cc.Strategy, strategy: newStrategy(cc.Strategy, opts),
			strategyOpts: opts, providers: ps, response: cc.Response,
		}
	}

	return &Router{
		combos: combos, defaultCombo: cfg.DefaultCombo,
		breakers: breakers, telemetry: tel, qualityHints: qualityHints,
	}, nil
}

// SetStrategy atomically replaces comboID's strategy for future rankings.
// Existing requests keep the immutable combo snapshot they already loaded,
// so changing strategy never races with or rewrites an in-flight attempt.
// Stateful strategy data (round-robin cursor, sticky/LKGP history) starts
// fresh because it belongs to the strategy instance being replaced.
func (r *Router) SetStrategy(comboID, name string) error {
	if !validStrategyName(name) || name == "" {
		return unknownStrategyError(comboID, name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.combos[comboID]
	if !ok {
		return fmt.Errorf("unknown combo %q", comboID)
	}
	if current.strategyName == name {
		return nil
	}

	next := *current
	next.strategyName = name
	next.strategy = newStrategy(name, current.strategyOpts)
	r.combos[comboID] = &next
	return nil
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
// providers were declared — the same order a non-scoring strategy's
// fallback follows.
func (r *Router) Combos() []ComboInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ComboInfo, 0, len(r.combos))
	for id, c := range r.combos {
		ids := make([]string, 0, len(c.providers))
		for _, p := range c.providers {
			ids = append(ids, p.ID())
		}
		out = append(out, ComboInfo{ID: id, Strategy: c.strategyName, Providers: ids})
	}
	return out
}

// Rank returns the full candidate ranking for one request against combo:
// hard constraints (circuit breaker, capability) are applied first (see
// eligibleCandidates), then the combo's configured Strategy ranks
// whatever's left. The returned RouteContext should be passed to
// RecordOutcome once the request finishes. runID is the caller's
// openai.RunIDHeader value ("" if absent) — see NewRouteContext.
func (r *Router) Rank(comboID string, req openai.ChatCompletionRequest, runID string) ([]RankedCandidate, RouteContext, error) {
	r.mu.RLock()
	c, ok := r.combos[comboID]
	r.mu.RUnlock()
	if !ok {
		return nil, RouteContext{}, fmt.Errorf("unknown combo %q", comboID)
	}

	ctx := NewRouteContext(comboID, req, runID)
	candidates := eligibleCandidates(c.providers, r.qualityHints, r.breakers, r.telemetry, ctx)
	if len(candidates) == 0 {
		return nil, ctx, fmt.Errorf("combo %q: no eligible providers (all circuit-open, or none support what this request needs)", comboID)
	}
	return c.strategy.Rank(ctx, candidates), ctx, nil
}

// RecordOutcome tells combo's strategy who actually won a request — only
// strategies that keep state between requests (the weighted family's
// sticky/LKGP, the standalone lkgp strategy) act on this; everything else
// is a no-op. Called by the attempt executor once a request finishes,
// never predicted ahead of time.
func (r *Router) RecordOutcome(comboID string, ctx RouteContext, winner string, ok bool) {
	r.mu.RLock()
	c, found := r.combos[comboID]
	r.mu.RUnlock()
	if !found {
		return
	}
	if rec, isRecorder := c.strategy.(outcomeRecorder); isRecorder {
		rec.RecordOutcome(ctx, winner, ok)
	}
}

// ResponseGateFor returns combo's configured ResponseGate. Unknown combos
// get a permissive (accept-everything) gate rather than an error, since
// callers already validate the combo ID via Resolve/Rank first.
func (r *Router) ResponseGateFor(comboID string) *ResponseGate {
	r.mu.RLock()
	c, ok := r.combos[comboID]
	r.mu.RUnlock()
	if !ok {
		return NewResponseGate(config.ResponseGateConfig{})
	}
	return NewResponseGate(c.response)
}

// StrategyName returns combo's configured strategy name (as written in
// config — "" and "priority" both mean declared order), for the wire
// response's Strategy field.
func (r *Router) StrategyName(comboID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.combos[comboID]; ok {
		return c.strategyName
	}
	return ""
}
