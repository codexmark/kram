package contextpolicy

import "testing"

func TestNewAllocatesFixedHistoryAndResponseFromOneWindow(t *testing.T) {
	p := New(60_000, 5_000)
	if p.ResponseReserveTokens != 7_500 || p.HistoryBudgetTokens != 47_500 {
		t.Fatalf("unexpected plan: %+v", p)
	}
	if p.FixedTokens+p.ResponseReserveTokens+p.HistoryBudgetTokens != p.MaxTokens {
		t.Fatalf("plan over/under-allocated its window: %+v", p)
	}
}

func TestActionChoosesCheapestSufficientIntervention(t *testing.T) {
	p := Plan{HistoryBudgetTokens: 100}
	if got := p.Action(90, 80); got != Keep {
		t.Errorf("under budget action = %v, want Keep", got)
	}
	if got := p.Action(120, 90); got != Prune {
		t.Errorf("prunable action = %v, want Prune", got)
	}
	if got := p.Action(120, 110); got != Compact {
		t.Errorf("still-over-budget action = %v, want Compact", got)
	}
}

func TestToolOutputBudgetUsesOnlyRemainingHistoryAllocation(t *testing.T) {
	p := Plan{HistoryBudgetTokens: 100}
	if got := p.ToolOutputBudgetChars(90, 1_000); got != 40 {
		t.Errorf("budget = %d, want 40 chars", got)
	}
	if got := p.ToolOutputBudgetChars(0, 200); got != 200 {
		t.Errorf("budget cap = %d, want 200", got)
	}
	if got := p.ToolOutputBudgetChars(110, 200); got != 0 {
		t.Errorf("over-budget result = %d, want 0", got)
	}
}
