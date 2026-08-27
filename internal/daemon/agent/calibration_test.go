package agent

import (
	"math"
	"testing"
)

func TestCalibratorDefaultsToOne(t *testing.T) {
	c := newTokenCalibrator()
	if got := c.factor("nope"); got != 1.0 {
		t.Errorf("unobserved session factor = %v, want 1.0", got)
	}
}

func TestCalibratorLearnsRatio(t *testing.T) {
	c := newTokenCalibrator()
	// Estimate 100, real 150 → the model tokenizes ~1.5× denser than chars/4.
	c.observe("s", 100, 150)
	if got := c.factor("s"); math.Abs(got-1.5) > 1e-9 {
		t.Errorf("factor after one observation = %v, want 1.5", got)
	}
}

func TestCalibratorClampsExtremes(t *testing.T) {
	c := newTokenCalibrator()
	c.observe("hi", 100, 100000) // absurd overreport
	if got := c.factor("hi"); got != maxCalibrationRatio {
		t.Errorf("high factor = %v, want clamped to %v", got, maxCalibrationRatio)
	}
	c.observe("lo", 100000, 100) // absurd underreport
	if got := c.factor("lo"); got != minCalibrationRatio {
		t.Errorf("low factor = %v, want clamped to %v", got, minCalibrationRatio)
	}
}

func TestCalibratorSmoothsAcrossObservations(t *testing.T) {
	c := newTokenCalibrator()
	c.observe("s", 100, 100) // ratio 1.0
	c.observe("s", 100, 200) // observed 2.0, EMA 0.5*2.0 + 0.5*1.0 = 1.5
	if got := c.factor("s"); math.Abs(got-1.5) > 1e-9 {
		t.Errorf("EMA factor = %v, want 1.5 (smoothed, not the raw 2.0)", got)
	}
}

func TestCalibratorIgnoresNonPositiveInputs(t *testing.T) {
	c := newTokenCalibrator()
	c.observe("s", 0, 100) // no estimate
	c.observe("s", 100, 0) // no usage reported
	c.observe("s", -5, -5) // garbage
	if got := c.factor("s"); got != 1.0 {
		t.Errorf("factor after only non-positive observations = %v, want 1.0", got)
	}
}

func TestCalibratorNilIsSafe(t *testing.T) {
	var c *tokenCalibrator // e.g. a Service built directly in a test
	if got := c.factor("s"); got != 1.0 {
		t.Errorf("nil calibrator factor = %v, want 1.0", got)
	}
	c.observe("s", 100, 200) // must not panic
}

// TestCalibratorBoundsEntries confirms the per-session map can't grow past
// its cap over a daemon's lifetime — inserting well beyond the bound keeps
// the map at the cap (evicting the least-recently-used), and a session
// touched recently survives eviction while stale ones are dropped.
func TestCalibratorBoundsEntries(t *testing.T) {
	c := newTokenCalibrator()
	// "keep" is observed first, then kept warm via factor() reads below.
	c.observe("keep", 100, 150)
	for i := 0; i < calibrationMaxEntries*2; i++ {
		c.observe(sessionKey(i), 100, 120)
		// Touch "keep" so it stays the most-recently-used and is never the
		// eviction victim.
		_ = c.factor("keep")
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > calibrationMaxEntries {
		t.Errorf("map grew to %d entries, want <= cap %d", n, calibrationMaxEntries)
	}
	if got := c.factor("keep"); got != 1.5 {
		t.Errorf("the continuously-used session was evicted (factor %v, want its learned 1.5)", got)
	}
}

func sessionKey(i int) string {
	return "s" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}

func TestScaleTokens(t *testing.T) {
	if got := scaleTokens(100, 1.5); got != 150 {
		t.Errorf("scaleTokens(100, 1.5) = %d, want 150", got)
	}
	if got := scaleTokens(3, 1.0); got != 3 {
		t.Errorf("scaleTokens(3, 1.0) = %d, want 3", got)
	}
	if got := scaleTokens(10, 1.25); got != 13 { // 12.5 rounds to 13
		t.Errorf("scaleTokens(10, 1.25) = %d, want 13 (rounded)", got)
	}
}
