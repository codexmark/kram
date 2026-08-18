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
	// failureThreshold is consecutive failures before a provider trips open.
	failureThreshold = 3
	// cooldown is how long a provider stays open before a trial half-open request.
	cooldown = 30 * time.Second
)

type entry struct {
	mu               sync.Mutex
	state            state
	consecutiveFails int
	openedAt         time.Time
}

// Registry tracks circuit breaker state for every known provider ID.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*entry
}

// NewRegistry creates an empty breaker registry; providers register lazily
// on first use.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*entry)}
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
// cooldown window (the half-open trial).
func (r *Registry) Allow(id string) bool {
	e := r.get(id)
	e.mu.Lock()
	defer e.mu.Unlock()

	switch e.state {
	case closed:
		return true
	case open:
		if time.Since(e.openedAt) >= cooldown {
			e.state = halfOpen
			return true
		}
		return false
	case halfOpen:
		// Only one trial in flight at a time; treat as allowed since the
		// caller is expected to report the outcome promptly.
		return true
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
		return
	}

	e.consecutiveFails++
	if e.consecutiveFails >= failureThreshold {
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
