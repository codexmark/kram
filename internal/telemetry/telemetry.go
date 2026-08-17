// Package telemetry tracks lightweight in-memory, per-provider counters so
// /admin/status can report what the gateway has been doing without needing
// an external metrics backend.
package telemetry

import "sync"

// ProviderStats is a point-in-time snapshot of one provider's counters.
type ProviderStats struct {
	Requests         int64 `json:"requests"`
	Failures         int64 `json:"failures"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type counters struct {
	mu               sync.Mutex
	requests         int64
	failures         int64
	promptTokens     int64
	completionTokens int64
}

// Registry aggregates counters per provider ID.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]*counters
}

// New creates an empty telemetry registry.
func New() *Registry {
	return &Registry{byID: make(map[string]*counters)}
}

func (r *Registry) get(id string) *counters {
	r.mu.RLock()
	c, ok := r.byID[id]
	r.mu.RUnlock()
	if ok {
		return c
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.byID[id]; ok {
		return c
	}
	c = &counters{}
	r.byID[id] = c
	return c
}

// RecordAttempt increments the request counter for a provider.
func (r *Registry) RecordAttempt(id string) {
	c := r.get(id)
	c.mu.Lock()
	c.requests++
	c.mu.Unlock()
}

// RecordFailure increments the failure counter for a provider.
func (r *Registry) RecordFailure(id string) {
	c := r.get(id)
	c.mu.Lock()
	c.failures++
	c.mu.Unlock()
}

// RecordUsage adds prompt/completion token counts for a provider.
func (r *Registry) RecordUsage(id string, promptTokens, completionTokens int) {
	c := r.get(id)
	c.mu.Lock()
	c.promptTokens += int64(promptTokens)
	c.completionTokens += int64(completionTokens)
	c.mu.Unlock()
}

// Snapshot returns a copy of all providers' current stats, keyed by ID.
func (r *Registry) Snapshot() map[string]ProviderStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]ProviderStats, len(r.byID))
	for id, c := range r.byID {
		c.mu.Lock()
		out[id] = ProviderStats{
			Requests:         c.requests,
			Failures:         c.failures,
			PromptTokens:     c.promptTokens,
			CompletionTokens: c.completionTokens,
		}
		c.mu.Unlock()
	}
	return out
}
