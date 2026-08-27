// Package breaker implements a per-provider circuit breaker so a single
// failing upstream never gets hammered with retries and never blocks
// requests from reaching healthy providers.
package breaker

import (
	"sync"
	"time"
)

type state int

const (
	closed state = iota
	open
	halfOpen
)

const (
	// defaultFailureThreshold is consecutive failures before a provider
	// trips open, when Config doesn't override it.
	defaultFailureThreshold = 3
	// defaultCooldown is how long a provider stays open before a trial
	// half-open request, when Config doesn't override it.
	defaultCooldown = 30 * time.Second
)

// Config tunes a Registry's breaker behavior. Zero values fall back to the
// defaults, so NewRegistryWithConfig(Config{}) == NewRegistry(). Fed from
// config.Tunables so slow local hardware can loosen the thresholds without
// a recompile.
type Config struct {
	// FailureThreshold is consecutive failures before a provider trips open.
	FailureThreshold int
	// Cooldown is how long a provider stays open before a half-open trial.
	Cooldown time.Duration
}

type entry struct {
	mu               sync.Mutex
	state            state
	consecutiveFails int
	openedAt         time.Time
	// trialAt is when the current half-open trial was admitted; the zero
	// value means no trial is currently in flight. Only meaningful while
	// state == halfOpen.
	trialAt time.Time
}

// Registry tracks circuit breaker state for every known provider ID.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*entry

	// failureThreshold and cooldown are resolved once at construction from
	// Config (or its defaults). halfOpenTrialTimeout equals cooldown — it
	// bounds how long an admitted half-open trial can hold the slot before
	// a fresh one is allowed in, a safety net for a trial whose caller
	// never reports an outcome (its goroutine panics, or the request is
	// abandoned via context cancellation before reaching ReportSuccess/
	// ReportFailure), so a missed report can't wedge the breaker in "trial
	// in flight" forever.
	failureThreshold int
	cooldown         time.Duration
}

// NewRegistry creates an empty breaker registry with the default
// thresholds; providers register lazily on first use.
func NewRegistry() *Registry {
	return NewRegistryWithConfig(Config{})
}

// NewRegistryWithConfig is NewRegistry with tunable thresholds; a zero field
// in cfg falls back to that field's default.
func NewRegistryWithConfig(cfg Config) *Registry {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = defaultFailureThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultCooldown
	}
	return &Registry{
		entries:          make(map[string]*entry),
		failureThreshold: cfg.FailureThreshold,
		cooldown:         cfg.Cooldown,
	}
}

func (r *Registry) get(id string) *entry {
	r.mu.RLock()
	e, ok := r.entries[id]
	r.mu.RUnlock()
	if ok {
		return e
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		return e
	}
	e = &entry{}
	r.entries[id] = e
	return e
}

// Allow reports whether a request may currently be attempted against the
// given provider. A provider in "open" state is allowed exactly once per
// cooldown window (the half-open trial) — and, once that trial is
// admitted, no further request is allowed until the trial's outcome is
// reported (or the trial timeout, which equals cooldown, elapses without
// one), so concurrent callers can never all pile onto an upstream that's
// still recovering.
func (r *Registry) Allow(id string) bool {
	e := r.get(id)
	e.mu.Lock()
	defer e.mu.Unlock()

	switch e.state {
	case closed:
		return true
	case open:
		if time.Since(e.openedAt) >= r.cooldown {
			e.state = halfOpen
			e.trialAt = time.Now()
			return true
		}
		return false
	case halfOpen:
		if e.trialAt.IsZero() || time.Since(e.trialAt) >= r.cooldown {
			e.trialAt = time.Now()
			return true
		}
		return false
	default:
		return true
	}
}

// ReportSuccess resets a provider back to a healthy, closed state.
func (r *Registry) ReportSuccess(id string) {
	e := r.get(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = closed
	e.consecutiveFails = 0
	e.trialAt = time.Time{}
}

// ReportFailure records a failed attempt, tripping the breaker open once
// the consecutive-failure threshold is reached (or immediately, if the
// failing attempt was itself a half-open trial).
func (r *Registry) ReportFailure(id string) {
	e := r.get(id)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state == halfOpen {
		e.state = open
		e.openedAt = time.Now()
		e.trialAt = time.Time{}
		return
	}

	e.consecutiveFails++
	if e.consecutiveFails >= r.failureThreshold {
		e.state = open
		e.openedAt = time.Now()
	}
}

// IsOpen reports whether a provider is currently tripped (for status/telemetry).
func (r *Registry) IsOpen(id string) bool {
	e := r.get(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == open
}

// IsHalfOpen reports whether a provider is currently in its half-open
// trial window — allowed through by Allow(), but not yet proven healthy
// again. Scoring strategies use this to treat a half-open candidate more
// cautiously than a fully closed one, without excluding it outright.
func (r *Registry) IsHalfOpen(id string) bool {
	e := r.get(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == halfOpen
}
