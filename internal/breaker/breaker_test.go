package breaker

import (
	"sync"
	"testing"
	"time"
)

func TestAllowClosedByDefault(t *testing.T) {
	r := NewRegistry()
	if !r.Allow("p") {
		t.Error("a never-seen provider should be allowed")
	}
	if r.IsOpen("p") || r.IsHalfOpen("p") {
		t.Error("a never-seen provider should be closed")
	}
}

func TestTripsOpenAtFailureThreshold(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < failureThreshold-1; i++ {
		r.ReportFailure("p")
		if r.IsOpen("p") {
			t.Fatalf("tripped open after only %d failures, want %d", i+1, failureThreshold)
		}
	}
	r.ReportFailure("p")
	if !r.IsOpen("p") {
		t.Error("expected breaker to be open after failureThreshold consecutive failures")
	}
	if r.Allow("p") {
		t.Error("an open breaker within its cooldown must not allow a request")
	}
}

func TestReportSuccessResetsToClosedAndClearsFailCount(t *testing.T) {
	r := NewRegistry()
	r.ReportFailure("p")
	r.ReportFailure("p")
	r.ReportSuccess("p")
	if r.IsOpen("p") {
		t.Error("ReportSuccess should reset an accumulating-failures provider to closed")
	}
	// consecutiveFails must also be reset, not just state — otherwise a
	// single failure right after a success would trip the breaker early.
	r.ReportFailure("p")
	if r.IsOpen("p") {
		t.Error("one failure right after ReportSuccess must not trip the breaker — consecutiveFails wasn't reset")
	}
}

func TestOpenTransitionsToHalfOpenAfterCooldown(t *testing.T) {
	r := NewRegistry()
	e := r.get("p")
	for i := 0; i < failureThreshold; i++ {
		r.ReportFailure("p")
	}
	if !r.IsOpen("p") {
		t.Fatal("setup: expected breaker to be open")
	}
	// Backdate openedAt instead of sleeping — cooldown is 30s, too slow
	// for a unit test to wait out for real.
	e.mu.Lock()
	e.openedAt = time.Now().Add(-cooldown - time.Second)
	e.mu.Unlock()

	if !r.Allow("p") {
		t.Error("expected the first Allow after cooldown to admit a half-open trial")
	}
	if !r.IsHalfOpen("p") {
		t.Error("expected state to be half-open after the cooldown-elapsed Allow")
	}
}

// TestHalfOpenIsSingleFlight is the regression test for the confirmed
// bug: Allow's halfOpen case returned true unconditionally regardless of
// how many trials were already in flight, contradicting its own comment
// ("only one trial in flight at a time"). Spin many concurrent callers
// while the breaker is half-open and assert exactly one is admitted.
func TestHalfOpenIsSingleFlight(t *testing.T) {
	r := NewRegistry()
	e := r.get("p")
	e.mu.Lock()
	e.state = halfOpen
	e.mu.Unlock()

	const concurrency = 50
	var admitted int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			if r.Allow("p") {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted != 1 {
		t.Errorf("expected exactly 1 concurrent caller admitted during half-open, got %d", admitted)
	}
}

func TestHalfOpenFailureReopensImmediately(t *testing.T) {
	r := NewRegistry()
	e := r.get("p")
	e.mu.Lock()
	e.state = halfOpen
	e.trialAt = time.Now()
	e.mu.Unlock()

	r.ReportFailure("p")
	if !r.IsOpen("p") {
		t.Error("a half-open trial's failure should reopen the breaker immediately, not wait for failureThreshold")
	}
	if r.Allow("p") {
		t.Error("freshly reopened breaker should not allow a request within cooldown")
	}
}

func TestHalfOpenSuccessClosesAndAllowsNewTrialSlotLater(t *testing.T) {
	r := NewRegistry()
	e := r.get("p")
	e.mu.Lock()
	e.state = halfOpen
	e.trialAt = time.Now()
	e.mu.Unlock()

	r.ReportSuccess("p")
	if r.IsOpen("p") || r.IsHalfOpen("p") {
		t.Error("a half-open trial's success should close the breaker")
	}
	if !r.Allow("p") {
		t.Error("a closed breaker should allow requests freely")
	}
}

// TestHalfOpenTrialTimeoutAdmitsFreshTrial covers the safety net: if a
// trial's outcome is never reported, a stuck breaker shouldn't block
// every request forever.
func TestHalfOpenTrialTimeoutAdmitsFreshTrial(t *testing.T) {
	r := NewRegistry()
	e := r.get("p")
	e.mu.Lock()
	e.state = halfOpen
	e.trialAt = time.Now().Add(-halfOpenTrialTimeout - time.Second) // simulate a trial whose outcome was never reported
	e.mu.Unlock()

	if !r.Allow("p") {
		t.Error("expected a fresh trial to be admitted once halfOpenTrialTimeout elapses with no reported outcome")
	}
}
