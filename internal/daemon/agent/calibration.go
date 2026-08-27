package agent

import (
	"math"
	"sync"
	"time"
)

// Token-estimate calibration. Kram sizes the compaction budget from a
// cheap chars/4 approximation (see compaction.EstimateTokens) because none
// of the providers behind the gateway share a tokenizer. chars/4 is
// systematically off — usually an underestimate for code, which tokenizes
// denser than prose — so the budget check can fire late (or, rarely, early)
// relative to the model's real window.
//
// But the gateway returns the real prompt_tokens on every response. The
// calibrator closes that loop: after each call it compares the raw estimate
// of what was sent against the real count, and stores a per-session
// correction factor that later budget checks multiply their estimates by.
// This is the "calibrate with prompt_tokens" path from the audit — 90% of a
// real per-provider tokenizer's benefit with none of the dependency.
const (
	// ratio bounds guard against a single pathological usage report (a
	// provider that reports 0, or an absurd value) whipsawing the budget.
	// A real chars/4 error lives comfortably inside this range.
	minCalibrationRatio = 0.5
	maxCalibrationRatio = 3.0
	// calibrationSmoothing weights a new observation against the running
	// value (EMA). 0.5 is responsive but still damps a one-off outlier.
	calibrationSmoothing = 0.5
	// calibrationMaxEntries bounds the per-session map so a daemon serving
	// thousands of short-lived sessions over its lifetime can't grow it
	// without limit — the same bounded-LRU treatment router.stickyStore
	// gives its own per-run map (see DECISIONS.md, "Sticky by default"). An
	// evicted session simply re-learns its factor from its next response,
	// exactly like a fresh one.
	calibrationMaxEntries = 256
)

type calibrationEntry struct {
	ratio    float64
	lastUsed time.Time
}

// tokenCalibrator tracks a per-session multiplier that corrects chars/4
// token estimates toward the model's real prompt_tokens. Safe for
// concurrent use — the daemon runs sessions independently.
type tokenCalibrator struct {
	mu      sync.Mutex
	entries map[string]calibrationEntry
}

func newTokenCalibrator() *tokenCalibrator {
	return &tokenCalibrator{entries: make(map[string]calibrationEntry)}
}

// observe records that a request whose raw chars/4 estimate was
// rawEstimate tokens actually cost realTokens prompt tokens, updating the
// session's correction factor. No-ops on a non-positive input (a missing
// usage report, or an empty request), so a provider that doesn't return
// usage simply leaves the factor at its last good value (or 1.0).
func (c *tokenCalibrator) observe(sessionID string, rawEstimate, realTokens int) {
	// nil is the "not calibrating" state — a Service built directly in a
	// test (not via New) has no calibrator and behaves exactly as it did
	// before calibration existed.
	if c == nil || rawEstimate <= 0 || realTokens <= 0 {
		return
	}
	observed := float64(realTokens) / float64(rawEstimate)
	if observed < minCalibrationRatio {
		observed = minCalibrationRatio
	}
	if observed > maxCalibrationRatio {
		observed = maxCalibrationRatio
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.entries[sessionID]; ok {
		observed = calibrationSmoothing*observed + (1-calibrationSmoothing)*prev.ratio
	} else if len(c.entries) >= calibrationMaxEntries {
		c.evictOldestLocked()
	}
	c.entries[sessionID] = calibrationEntry{ratio: observed, lastUsed: time.Now()}
}

// factor returns the session's correction multiplier, or 1.0 if nothing
// has been observed yet (the pre-calibration behavior — raw chars/4). A
// read counts as a use for the LRU bound, so an actively-running session is
// never the one evicted.
func (c *tokenCalibrator) factor(sessionID string) float64 {
	if c == nil {
		return 1.0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[sessionID]; ok {
		e.lastUsed = time.Now()
		c.entries[sessionID] = e
		return e.ratio
	}
	return 1.0
}

// evictOldestLocked drops the least-recently-used entry. Caller holds mu.
func (c *tokenCalibrator) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, e := range c.entries {
		if first || e.lastUsed.Before(oldestTime) {
			oldestKey, oldestTime, first = k, e.lastUsed, false
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// scaleTokens applies a calibration factor to a raw token estimate,
// rounding to the nearest whole token.
func scaleTokens(n int, factor float64) int {
	return int(math.Round(float64(n) * factor))
}
