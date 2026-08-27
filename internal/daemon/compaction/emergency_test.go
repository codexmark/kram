package compaction

import (
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/openai"
)

func turn(user, assistant string) []store.Message {
	return []store.Message{
		{Role: "user", Content: user},
		{Role: "assistant", Content: assistant},
	}
}

func TestEmergencyPruneKeepsNewestWholeTurnsWithinBudget(t *testing.T) {
	big := strings.Repeat("x", 4000)
	var msgs []store.Message
	msgs = append(msgs, turn("q1 "+big, "a1 "+big)...)
	msgs = append(msgs, turn("q2 "+big, "a2 "+big)...)
	msgs = append(msgs, turn("q3", "a3")...)

	out := EmergencyPrune(msgs, EstimateTokens(msgs[2:])) // budget fits turns 2+3, not 1

	if len(out) != 4 || !strings.HasPrefix(out[0].Content, "q2") {
		t.Fatalf("expected turns 2+3 to survive, got %d messages starting %q", len(out), out[0].Content)
	}
}

func TestEmergencyPruneNeverSplitsToolCallPairs(t *testing.T) {
	msgs := []store.Message{
		{Role: "user", Content: "old " + strings.Repeat("x", 8000)},
		{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: "result"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "new question"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: "c2"}}},
		{Role: "tool", ToolCallID: "c2", Content: "result2"},
		{Role: "assistant", Content: "new answer"},
	}
	out := EmergencyPrune(msgs, 100) // only the newest turn fits

	if out[0].Role != "user" || out[0].Content != "new question" {
		t.Fatalf("cut must land on a user boundary, got first = %+v", out[0])
	}
	// The surviving suffix carries its tool pair intact.
	var sawCall, sawResult bool
	for _, m := range out {
		if len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "c2" {
			sawCall = true
		}
		if m.Role == "tool" && m.ToolCallID == "c2" {
			sawResult = true
		}
		if m.Role == "tool" && m.ToolCallID == "c1" {
			t.Fatal("orphaned tool result from the dropped turn survived")
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("surviving turn lost its tool pair: call=%v result=%v", sawCall, sawResult)
	}
}

func TestEmergencyPruneKeepsLeadingMarkerAndNeverReturnsEmpty(t *testing.T) {
	msgs := []store.Message{
		{Role: "system", Name: CompactionMarkerName, Content: "earlier summary"},
		{Role: "user", Content: strings.Repeat("x", 9000)},
		{Role: "assistant", Content: strings.Repeat("y", 9000)},
	}
	out := EmergencyPrune(msgs, 10) // nothing truly fits

	if len(out) != 3 {
		t.Fatalf("the marker plus the newest turn must survive even over budget, got %d", len(out))
	}
	if out[0].Name != CompactionMarkerName {
		t.Fatalf("leading compaction marker lost: %+v", out[0])
	}
}
