package telemetry

import (
	"sync"
	"testing"
)

func TestSnapshotOfEmptyRegistryIsEmpty(t *testing.T) {
	r := New()
	snap := r.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot() of a fresh registry = %+v, want empty", snap)
	}
}

func TestRecordAttemptIncrementsRequests(t *testing.T) {
	r := New()
	r.RecordAttempt("p1")
	r.RecordAttempt("p1")

	stats := r.Snapshot()["p1"]
	if stats.Requests != 2 {
		t.Errorf("Requests = %d, want 2", stats.Requests)
	}
}

func TestRecordFailureIncrementsFailures(t *testing.T) {
	r := New()
	r.RecordAttempt("p1")
	r.RecordAttempt("p1")
	r.RecordFailure("p1")

	stats := r.Snapshot()["p1"]
	if stats.Failures != 1 {
		t.Errorf("Failures = %d, want 1", stats.Failures)
	}
}

func TestRecordUsageAccumulatesTokens(t *testing.T) {
	r := New()
	r.RecordUsage("p1", 100, 50)
	r.RecordUsage("p1", 20, 10)

	stats := r.Snapshot()["p1"]
	if stats.PromptTokens != 120 || stats.CompletionTokens != 60 {
		t.Errorf("stats = %+v, want PromptTokens=120 CompletionTokens=60", stats)
	}
}

func TestRecordLatencyComputesRunningAverage(t *testing.T) {
	r := New()
	r.RecordLatency("p1", 100)
	r.RecordLatency("p1", 300)

	stats := r.Snapshot()["p1"]
	if stats.AvgLatencyMS != 200 {
		t.Errorf("AvgLatencyMS = %d, want 200 (average of 100 and 300)", stats.AvgLatencyMS)
	}
}

func TestSnapshotAvgLatencyZeroWhenNoneRecorded(t *testing.T) {
	r := New()
	r.RecordAttempt("p1")

	stats := r.Snapshot()["p1"]
	if stats.AvgLatencyMS != 0 {
		t.Errorf("AvgLatencyMS = %d, want 0 when no latency was ever recorded", stats.AvgLatencyMS)
	}
}

func TestSnapshotSuccessRateComputation(t *testing.T) {
	r := New()
	r.RecordAttempt("p1")
	r.RecordAttempt("p1")
	r.RecordAttempt("p1")
	r.RecordAttempt("p1")
	r.RecordFailure("p1")

	stats := r.Snapshot()["p1"]
	if stats.SuccessRate != 0.75 {
		t.Errorf("SuccessRate = %v, want 0.75 (3 of 4 succeeded)", stats.SuccessRate)
	}
}

func TestSnapshotSuccessRateZeroWhenNoRequests(t *testing.T) {
	r := New()
	r.RecordLatency("p1", 5) // touches the provider without ever recording a request
	stats := r.Snapshot()["p1"]
	if stats.SuccessRate != 0 {
		t.Errorf("SuccessRate = %v, want 0 when Requests is 0 (avoids a divide-by-zero)", stats.SuccessRate)
	}
}

func TestSnapshotKeepsProvidersIndependent(t *testing.T) {
	r := New()
	r.RecordAttempt("p1")
	r.RecordAttempt("p2")
	r.RecordFailure("p2")

	snap := r.Snapshot()
	if snap["p1"].Requests != 1 || snap["p1"].Failures != 0 {
		t.Errorf("p1 stats = %+v, want Requests=1 Failures=0", snap["p1"])
	}
	if snap["p2"].Requests != 1 || snap["p2"].Failures != 1 {
		t.Errorf("p2 stats = %+v, want Requests=1 Failures=1", snap["p2"])
	}
}

func TestSnapshotReturnsACopyNotLiveCounters(t *testing.T) {
	r := New()
	r.RecordAttempt("p1")
	snap := r.Snapshot()

	r.RecordAttempt("p1")

	if snap["p1"].Requests != 1 {
		t.Errorf("earlier snapshot's Requests changed to %d after a later RecordAttempt — Snapshot must return an independent copy", snap["p1"].Requests)
	}
}

// TestRegistryIsSafeForConcurrentUse exercises the lock/RLock split in
// get() under the race detector — the whole reason counters is guarded
// by its own mutex separate from the registry's map lock.
func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RecordAttempt("p1")
			r.RecordFailure("p1")
			r.RecordUsage("p1", 1, 1)
			r.RecordLatency("p1", 10)
			_ = r.Snapshot()
		}()
	}
	wg.Wait()

	stats := r.Snapshot()["p1"]
	if stats.Requests != 50 {
		t.Errorf("Requests = %d, want 50 after 50 concurrent RecordAttempt calls", stats.Requests)
	}
}
