package permission

import "testing"

func TestDefaultAllowsWhenNoPolicy(t *testing.T) {
	e := NewEvaluator(PolicyFile{}, nil)
	if got := e.Evaluate("bash", "rm -rf /"); got != Allow {
		t.Errorf("compatibility default: got %s, want allow", got)
	}
}

func TestExactToolExactPatternBeatsWildcard(t *testing.T) {
	policy := PolicyFile{Rules: []Rule{
		{Tool: "bash", Pattern: "*", Decision: Allow},
		{Tool: "bash", Pattern: "git push*", Decision: Ask},
	}}
	e := NewEvaluator(policy, nil)

	if got := e.Evaluate("bash", "git push origin main"); got != Ask {
		t.Errorf("specific pattern should beat wildcard: got %s, want ask", got)
	}
	if got := e.Evaluate("bash", "go test ./..."); got != Allow {
		t.Errorf("non-matching command should fall back to wildcard: got %s, want allow", got)
	}
}

func TestExactToolBeatsGlobTool(t *testing.T) {
	policy := PolicyFile{Rules: []Rule{
		{Tool: "mcp__*", Decision: Ask},
		{Tool: "mcp__github__create_issue", Decision: Allow},
	}}
	e := NewEvaluator(policy, nil)

	if got := e.Evaluate("mcp__github__create_issue", ""); got != Allow {
		t.Errorf("exact tool name should beat glob: got %s, want allow", got)
	}
	if got := e.Evaluate("mcp__github__delete_repo", ""); got != Ask {
		t.Errorf("unmatched exact tool should fall back to glob: got %s, want ask", got)
	}
}

func TestDenyBlocks(t *testing.T) {
	policy := PolicyFile{Rules: []Rule{
		{Tool: "bash", Pattern: "rm -rf *", Decision: Deny},
	}}
	e := NewEvaluator(policy, nil)
	if got := e.Evaluate("bash", "rm -rf /"); got != Deny {
		t.Errorf("got %s, want deny", got)
	}
}

func TestPartialDenyKeepsToolVisible(t *testing.T) {
	policy := PolicyFile{Rules: []Rule{
		{Tool: "bash", Pattern: "rm -rf *", Decision: Deny},
		{Tool: "bash", Pattern: "*", Decision: Allow},
	}}
	e := NewEvaluator(policy, nil)
	if e.FullyDenied("bash") {
		t.Error("a tool with a non-deny rule alongside a deny rule should not be FullyDenied")
	}
}

func TestBlanketDenyHidesTool(t *testing.T) {
	policy := PolicyFile{Rules: []Rule{
		{Tool: "bash", Pattern: "*", Decision: Deny},
	}}
	e := NewEvaluator(policy, nil)
	if !e.FullyDenied("bash") {
		t.Error("a tool with only deny rules should be FullyDenied")
	}
}

func TestDefaultDenyHidesUnmentionedTool(t *testing.T) {
	policy := PolicyFile{Default: Deny}
	e := NewEvaluator(policy, nil)
	if !e.FullyDenied("some_random_tool") {
		t.Error("a global default of deny should make an unmentioned tool FullyDenied")
	}
}

func TestFullyDeniedFalseWhenAllowedElsewhere(t *testing.T) {
	e := NewEvaluator(PolicyFile{}, nil)
	if e.FullyDenied("bash") {
		t.Error("compatibility default (allow) must never report a tool as FullyDenied")
	}
}

func TestGrantOverridesSameSpecificityAsk(t *testing.T) {
	policy := PolicyFile{Rules: []Rule{
		{Tool: "delegate_task", Decision: Ask},
	}}
	grants := []Rule{{Tool: "delegate_task", Pattern: "", Decision: Allow}}
	e := NewEvaluator(policy, grants)

	if got := e.Evaluate("delegate_task", ""); got != Allow {
		t.Errorf("a same-specificity grant should override the configured ask: got %s, want allow", got)
	}
}

func TestGrantIsExactNotWildcard(t *testing.T) {
	// A grant for one exact command must not leak into approving a
	// different command for the same tool — this is the "always allow
	// bash *" danger the design explicitly avoids.
	policy := PolicyFile{Rules: []Rule{
		{Tool: "bash", Pattern: "*", Decision: Ask},
	}}
	grants := []Rule{{Tool: "bash", Pattern: "git push origin feature/foo", Decision: Allow}}
	e := NewEvaluator(policy, grants)

	if got := e.Evaluate("bash", "git push origin feature/foo"); got != Allow {
		t.Errorf("the exact granted command should be allowed: got %s", got)
	}
	if got := e.Evaluate("bash", "git push origin main"); got != Ask {
		t.Errorf("a stale/different command must still ask, got %s, want ask", got)
	}
	if got := e.Evaluate("bash", "rm -rf /"); got != Ask {
		t.Errorf("an unrelated command must still ask, got %s, want ask", got)
	}
}
