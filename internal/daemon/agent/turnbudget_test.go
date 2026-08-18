package agent

import (
	"strings"
	"testing"
)

func TestEnforceTurnOutputBudgetUnderBudgetPassesThrough(t *testing.T) {
	out, hit := enforceTurnOutputBudget("small result", 0, 180_000)
	if hit {
		t.Error("content well under budget should not be flagged as hit")
	}
	if out != "small result" {
		t.Errorf("content under budget should pass through unchanged, got %q", out)
	}
}

func TestEnforceTurnOutputBudgetTruncatesWhenCrossingLimit(t *testing.T) {
	content := make([]byte, 100)
	for i := range content {
		content[i] = 'a'
	}
	out, hit := enforceTurnOutputBudget(string(content), 950, 1000)
	if !hit {
		t.Fatal("expected the budget to be hit")
	}
	if len(out) <= 50 {
		t.Errorf("truncated output should still carry the truncated content plus a notice, got %q", out)
	}
	// Only 50 of the 100 chars should have fit.
	kept := out[:50]
	for _, c := range kept {
		if c != 'a' {
			t.Fatalf("expected the first 50 chars to be the real content, got %q", kept)
		}
	}
}

func TestEnforceTurnOutputBudgetNeverSilentlyEmpty(t *testing.T) {
	out, hit := enforceTurnOutputBudget("this result arrives after the budget is already spent", 180_000, 180_000)
	if !hit {
		t.Fatal("expected the budget to already be exhausted")
	}
	if out == "" {
		t.Fatal("a budget-exhausted result must explain itself, never return empty text")
	}
}

func TestEnforceTurnOutputBudgetAccumulatesAcrossCalls(t *testing.T) {
	budget := 100
	used := 0

	first := "0123456789" // 10 chars
	out1, hit1 := enforceTurnOutputBudget(first, used, budget)
	if hit1 {
		t.Fatal("first small call should fit")
	}
	used += len(first)

	second := make([]byte, 95)
	for i := range second {
		second[i] = 'b'
	}
	out2, hit2 := enforceTurnOutputBudget(string(second), used, budget)
	if !hit2 {
		t.Fatal("second call should push the running total over budget and be truncated")
	}
	// Only budget-used=90 bytes of real content should have survived,
	// even though the returned string is longer once the explanatory
	// notice is appended — the notice isn't a silent truncation, it's the
	// whole point.
	wantKept := budget - used
	if len(out2) < wantKept || out2[:wantKept] != string(second[:wantKept]) {
		t.Errorf("expected exactly %d bytes of real content preserved as a prefix, got %q", wantKept, out2)
	}
	if strings.Contains(out2, string(second)) {
		t.Error("the full original content should not have survived — only the remaining budget's worth")
	}
	_ = out1
}
