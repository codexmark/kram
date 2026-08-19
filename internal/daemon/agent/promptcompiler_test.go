package agent

import (
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func TestCompilePreambleBaseOnly(t *testing.T) {
	parts := compilePreamble("/ws", "", false, openai.ChatMessage{}, false)

	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1 (base only): %+v", len(parts), parts)
	}
	p := parts[0]
	if p.ID != "base" || p.Placement != PlacementPreamble || p.Refresh != RefreshStatic || p.Source != "builtin" {
		t.Errorf("base part = %+v, want ID=base Placement=Preamble Refresh=Static Source=builtin", p)
	}
	if !strings.Contains(p.Content, "Kram") {
		t.Errorf("base part content doesn't look like systemPrompt output: %q", p.Content[:min(80, len(p.Content))])
	}
}

func TestCompilePreambleWithProjectContextOnly(t *testing.T) {
	parts := compilePreamble("/ws", "some AGENTS.md text", true, openai.ChatMessage{}, false)

	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (base + project-context): %+v", len(parts), parts)
	}
	p := parts[1]
	if p.ID != "project-context" || p.Placement != PlacementPreamble || p.Refresh != RefreshIteration || p.Source != "AGENTS.md" {
		t.Errorf("project-context part = %+v, want ID=project-context Placement=Preamble Refresh=Iteration Source=AGENTS.md", p)
	}
	if !strings.Contains(p.Content, "some AGENTS.md text") {
		t.Errorf("project-context content missing the actual text: %q", p.Content)
	}
	if !strings.Contains(p.Content, "AGENTS.md/CLAUDE.md") {
		t.Errorf("project-context content missing the framing header: %q", p.Content)
	}
}

func TestCompilePreambleWithMemoryOnly(t *testing.T) {
	memMsg := openai.ChatMessage{Role: "system", Content: "remembered fact"}
	parts := compilePreamble("/ws", "", false, memMsg, true)

	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (base + memory): %+v", len(parts), parts)
	}
	p := parts[1]
	if p.ID != "memory" || p.Placement != PlacementPreamble || p.Refresh != RefreshRun || p.Source != "memory" {
		t.Errorf("memory part = %+v, want ID=memory Placement=Preamble Refresh=Run Source=memory", p)
	}
	if p.Content != "remembered fact" {
		t.Errorf("memory part content = %q, want the memoryMsg's own Content passed through unchanged", p.Content)
	}
}

func TestCompilePreambleWithBothProjectContextAndMemory(t *testing.T) {
	memMsg := openai.ChatMessage{Content: "remembered fact"}
	parts := compilePreamble("/ws", "ctx", true, memMsg, true)

	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3 (base, project-context, memory): %+v", len(parts), parts)
	}
	gotIDs := []string{parts[0].ID, parts[1].ID, parts[2].ID}
	wantIDs := []string{"base", "project-context", "memory"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("order[%d] = %q, want %q (full order: %v)", i, gotIDs[i], wantIDs[i], gotIDs)
		}
	}
}

func TestCompileTurnPostscriptNeitherFlag(t *testing.T) {
	parts := compileTurnPostscript(false, false)
	if len(parts) != 0 {
		t.Errorf("expected 0 parts when neither flag is set, got %+v", parts)
	}
}

func TestCompileTurnPostscriptEmptyRetryOnly(t *testing.T) {
	parts := compileTurnPostscript(true, false)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.ID != "empty-retry-nudge" || p.Placement != PlacementPostHistory || p.Refresh != RefreshIteration || p.Source != "runtime" {
		t.Errorf("part = %+v, want ID=empty-retry-nudge Placement=PostHistory Refresh=Iteration Source=runtime", p)
	}
	if !strings.Contains(p.Content, "empty") {
		t.Errorf("empty-retry content doesn't mention the empty response: %q", p.Content)
	}
}

func TestCompileTurnPostscriptNearBudgetOnly(t *testing.T) {
	parts := compileTurnPostscript(false, true)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.ID != "turn-budget-soft-landing" || p.Placement != PlacementPostHistory || p.Refresh != RefreshIteration || p.Source != "runtime" {
		t.Errorf("part = %+v, want ID=turn-budget-soft-landing Placement=PostHistory Refresh=Iteration Source=runtime", p)
	}
}

// TestCompileTurnPostscriptBothFlagsPreservesOrder pins the exact order
// the original inline code produced: the empty-retry nudge was appended
// immediately after history, the near-budget message later (after
// toolDefs was computed) — both land after history, empty-retry first.
func TestCompileTurnPostscriptBothFlagsPreservesOrder(t *testing.T) {
	parts := compileTurnPostscript(true, true)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2: %+v", len(parts), parts)
	}
	if parts[0].ID != "empty-retry-nudge" || parts[1].ID != "turn-budget-soft-landing" {
		t.Errorf("order = [%s, %s], want [empty-retry-nudge, turn-budget-soft-landing]", parts[0].ID, parts[1].ID)
	}
}

func TestPartsToMessagesRendersSystemRoleInOrder(t *testing.T) {
	parts := []PromptPart{
		{ID: "a", Content: "first"},
		{ID: "b", Content: "second"},
	}
	msgs := partsToMessages(parts)

	if len(msgs) != 2 {
		t.Fatalf("msgs = %d, want 2", len(msgs))
	}
	for i, want := range []string{"first", "second"} {
		if msgs[i].Role != "system" {
			t.Errorf("msgs[%d].Role = %q, want system", i, msgs[i].Role)
		}
		if msgs[i].Content != want {
			t.Errorf("msgs[%d].Content = %q, want %q", i, msgs[i].Content, want)
		}
	}
}

func TestPartsToMessagesEmptyInputProducesEmptyOutput(t *testing.T) {
	msgs := partsToMessages(nil)
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages for nil input, got %d", len(msgs))
	}
}
