package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

// TestBuildGeminiContentsToolRoleIsUser guards against the exact bug found
// live: Gemini's real API rejects role "function" outright ("Role
// 'function' is not supported"), despite that being the name Kram's own
// normalized tool-result role uses internally.
func TestBuildGeminiContentsToolRoleIsUser(t *testing.T) {
	_, contents := buildGeminiContents([]openai.ChatMessage{
		{Role: "tool", Name: "list_dir", Content: `{"result":"a.txt"}`, ToolCallID: "call_1"},
	})
	if len(contents) != 1 {
		t.Fatalf("expected 1 content entry, got %d", len(contents))
	}
	if contents[0].Role != "user" {
		t.Errorf("tool result role = %q, want %q (Gemini rejects \"function\" as an invalid role)", contents[0].Role, "user")
	}
	if len(contents[0].Parts) != 1 || contents[0].Parts[0].FunctionResp == nil {
		t.Fatalf("expected exactly one functionResponse part, got %+v", contents[0].Parts)
	}
	if contents[0].Parts[0].FunctionResp.Name != "list_dir" {
		t.Errorf("functionResponse name = %q, want list_dir", contents[0].Parts[0].FunctionResp.Name)
	}
}

// TestBuildGeminiContentsPropagatesThoughtSignature guards against the
// second live-found bug: a thinking-enabled Gemini model requires its own
// thoughtSignature echoed back on the corresponding functionCall part in
// later turns, or it rejects the request outright. This confirms the
// value carried on openai.ToolCall.GeminiThoughtSignature survives the
// translation into the outgoing request.
func TestBuildGeminiContentsPropagatesThoughtSignature(t *testing.T) {
	_, contents := buildGeminiContents([]openai.ChatMessage{
		{Role: "assistant", ToolCalls: []openai.ToolCall{
			{
				ID:                     "call_1",
				Type:                   "function",
				Function:               openai.ToolCallFunction{Name: "list_dir", Arguments: "{}"},
				GeminiThoughtSignature: "opaque-signature-bytes",
			},
		}},
	})
	if len(contents) != 1 {
		t.Fatalf("expected 1 content entry, got %d", len(contents))
	}
	if len(contents[0].Parts) != 1 || contents[0].Parts[0].FunctionCall == nil {
		t.Fatalf("expected exactly one functionCall part, got %+v", contents[0].Parts)
	}
	if got := contents[0].Parts[0].ThoughtSignature; got != "opaque-signature-bytes" {
		t.Errorf("ThoughtSignature = %q, want it echoed back unchanged", got)
	}
}

// TestBuildGeminiContentsAssistantWithoutSignatureOmitsIt confirms a tool
// call with no captured signature (any non-Gemini provider, or a
// non-thinking Gemini model) doesn't fabricate one — an empty value must
// stay empty, not get replaced with some placeholder that would itself
// fail Gemini's base64 validation.
func TestBuildGeminiContentsAssistantWithoutSignatureOmitsIt(t *testing.T) {
	_, contents := buildGeminiContents([]openai.ChatMessage{
		{Role: "assistant", ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: "function", Function: openai.ToolCallFunction{Name: "list_dir", Arguments: "{}"}},
		}},
	})
	if got := contents[0].Parts[0].ThoughtSignature; got != "" {
		t.Errorf("ThoughtSignature = %q, want empty when none was captured", got)
	}
}

// TestGeminiEmitsToolCallProgress is the regression test for the
// confirmed liveness gap: a functionCall part used to be accumulated
// into toolCalls with no matching StreamEvent — Gemini sends a whole
// function call in one part rather than fragmented deltas, but the
// liveness gap is the same: a tool-calling turn with no leading text
// produced zero events until Done.
func TestGeminiEmitsToolCallProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_dir","args":{}}}]}}]}`)
	}))
	defer srv.Close()

	p := NewGemini("test", srv.URL, "key", "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var sawProgress bool
	for e := range events {
		if e.ToolCallProgress {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Error("expected a ToolCallProgress event for the functionCall part")
	}
}
