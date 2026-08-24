package provider

import (
	"reflect"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func TestToolCallAccumulatorFinishReturnsNilWhenEmpty(t *testing.T) {
	a := newToolCallAccumulator()
	if got := a.finish(); got != nil {
		t.Errorf("finish() on an empty accumulator = %+v, want nil", got)
	}
}

func TestToolCallAccumulatorAssemblesFragmentedArguments(t *testing.T) {
	a := newToolCallAccumulator()
	a.add(0, "call_1", "list_dir", `{"pa`)
	a.add(0, "", "", `th":"/tmp"}`)

	got := a.finish()
	want := []openai.ToolCall{{ID: "call_1", Type: "function", Function: openai.ToolCallFunction{Name: "list_dir", Arguments: `{"path":"/tmp"}`}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("finish() = %+v, want %+v", got, want)
	}
}

// TestToolCallAccumulatorOrdersByIndex confirms finish() sorts by index
// rather than insertion order — providers can emit later-indexed tool
// calls' fragments before earlier ones finish.
func TestToolCallAccumulatorOrdersByIndex(t *testing.T) {
	a := newToolCallAccumulator()
	a.add(1, "call_2", "grep", "{}")
	a.add(0, "call_1", "read_file", "{}")

	got := a.finish()
	if len(got) != 2 || got[0].ID != "call_1" || got[1].ID != "call_2" {
		t.Errorf("finish() = %+v, want [call_1, call_2] in index order", got)
	}
}

// TestToolCallAccumulatorLaterEmptyFragmentsDoNotOverwriteIDOrName
// confirms add() only overwrites ID/Name when the fragment actually
// carries one — later argument-only fragments (empty id/name) must not
// blank out what an earlier fragment already set.
func TestToolCallAccumulatorLaterEmptyFragmentsDoNotOverwriteIDOrName(t *testing.T) {
	a := newToolCallAccumulator()
	a.add(0, "call_1", "bash", "")
	a.add(0, "", "", `{"cmd":"ls"}`)

	got := a.finish()
	if len(got) != 1 || got[0].ID != "call_1" || got[0].Function.Name != "bash" {
		t.Errorf("finish() = %+v, want ID/Name preserved from the first fragment", got)
	}
}

func TestToolCallAccumulatorRejectsMalformedAndPhantomCalls(t *testing.T) {
	a := newToolCallAccumulator()
	a.add(0, "call_good", "skill_list", "")
	a.add(1, "call_bad", "run_background", `{"command":"truncated`)
	a.add(2, "", "", `{}`)

	got := a.finish()
	if len(got) != 1 || got[0].ID != "call_good" || got[0].Function.Arguments != `{}` {
		t.Fatalf("finish() = %+v, want only normalized call_good", got)
	}
}

func TestSanitizeToolHistoryDropsInvalidPairsAndKeepsValidContext(t *testing.T) {
	messages := []openai.ChatMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{
			{ID: "good", Type: "function", Function: openai.ToolCallFunction{Name: "skill_list", Arguments: ""}},
			{ID: "bad", Type: "function", Function: openai.ToolCallFunction{Name: "run_background", Arguments: `{"command":"cut`}},
			{ID: "", Type: "function", Function: openai.ToolCallFunction{Name: "", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "good", Name: "skill_list", Content: "[]"},
		{Role: "tool", ToolCallID: "bad", Name: "run_background", Content: "invalid arguments"},
		{Role: "assistant", Content: "still useful", ToolCalls: []openai.ToolCall{{ID: "bad2", Function: openai.ToolCallFunction{Name: "bash", Arguments: `[`}}}},
	}

	got := sanitizeToolHistory(messages)
	if len(got) != 4 {
		t.Fatalf("sanitizeToolHistory() len = %d, want 4: %+v", len(got), got)
	}
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].ID != "good" || got[1].ToolCalls[0].Function.Arguments != `{}` {
		t.Fatalf("valid call was not preserved and normalized: %+v", got[1])
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "good" {
		t.Fatalf("valid tool result was not preserved: %+v", got[2])
	}
	if got[3].Content != "still useful" || len(got[3].ToolCalls) != 0 {
		t.Fatalf("assistant text should survive invalid sibling call: %+v", got[3])
	}
}
