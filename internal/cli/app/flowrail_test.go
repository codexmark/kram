package app

import (
	"strings"
	"testing"
	"time"
)

func TestFlowRailDirections(t *testing.T) {
	now := time.Now()

	rx := Model{flowRxAt: now}
	rail, ok := rx.renderFlowRail()
	if !ok || !strings.Contains(rail, "»") || strings.Contains(rail, "«") {
		t.Fatalf("receiving rail = %q ok=%v, want rightward glyphs only", rail, ok)
	}

	tx := Model{flowTxAt: now}
	rail, ok = tx.renderFlowRail()
	if !ok || !strings.Contains(rail, "«") || strings.Contains(rail, "»") {
		t.Fatalf("sending rail = %q ok=%v, want leftward glyphs only", rail, ok)
	}

	both := Model{flowRxAt: now, flowTxAt: now}
	rail, ok = both.renderFlowRail()
	if !ok || !strings.Contains(rail, "»") || !strings.Contains(rail, "«") {
		t.Fatalf("convergence rail = %q ok=%v, want both directions", rail, ok)
	}

	// Sending phase alone (route_start fired, no bytes back yet) counts
	// as outbound even with a stale flowTxAt.
	sending := Model{flowSending: true}
	if _, ok := sending.renderFlowRail(); !ok {
		t.Fatal("flowSending must keep the leftward rail active")
	}

	// Stale timestamps: no direction — caller falls back to the neutral pulse.
	stale := Model{flowRxAt: now.Add(-2 * time.Second), flowTxAt: now.Add(-2 * time.Second)}
	if rail, ok := stale.renderFlowRail(); ok {
		t.Fatalf("stale flow should be inactive, got %q", rail)
	}
	neutral := stale.renderActivityRail(false)
	if !strings.Contains(neutral, "●") {
		t.Fatalf("idle rail lost its neutral pulse: %q", neutral)
	}
}

func TestFlowHeatBuckets(t *testing.T) {
	for rate, want := range map[float64]string{
		10: string(flowHeatGreen), 300: string(flowHeatYellow),
		2000: string(flowHeatOrange), 9000: string(flowHeatHot),
	} {
		if got := string(flowHeatColor(rate)); got != want {
			t.Errorf("heat(%v) = %s, want %s", rate, got, want)
		}
	}
}

func TestNoteFlowBytesSmoothsRate(t *testing.T) {
	m := Model{}
	m.noteFlowRx(100)
	if m.flowRate != 0 {
		t.Fatalf("rate before the window elapses = %v, want 0", m.flowRate)
	}
	// Force the window to have elapsed, then land more bytes: the rate
	// folds in (500 bytes over ~1s → ~250..600 B/s after smoothing).
	m.flowRateAt = time.Now().Add(-time.Second)
	m.noteFlowRx(400)
	if m.flowRate < 100 || m.flowRate > 600 {
		t.Fatalf("smoothed rate = %v, want a plausible bytes/sec figure", m.flowRate)
	}
	// receiving ends the sending phase
	m.flowSending = true
	m.noteFlowRx(1)
	if m.flowSending {
		t.Fatal("first received byte must end the sending phase")
	}
}
