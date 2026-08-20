package provider

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func representativeMessages() []openai.ChatMessage {
	call := openai.ToolCall{ID: "call-1", Type: "function", Function: openai.ToolCallFunction{Name: "lookup", Arguments: `{"q":"go"}`}, GeminiThoughtSignature: "opaque"}
	return []openai.ChatMessage{
		{Role: "system", Content: "first"},
		{Role: "system", Content: "second"},
		{Role: "user", Content: "look", Images: []string{
			"data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("png")),
			"https://example.invalid/not-data", "data:image/png;base64,%%%",
		}},
		{Role: "assistant", Content: "working", ToolCalls: []openai.ToolCall{call, {ID: "empty", Function: openai.ToolCallFunction{Name: "noop"}}}},
		{Role: "tool", ToolCallID: "call-1", Name: "lookup", Content: `{"value":42}`},
	}
}

func representativeTools() []openai.Tool {
	return []openai.Tool{{Type: "function", Function: openai.ToolFunction{Name: "lookup", Description: "find it", Parameters: json.RawMessage(`{"type":"object"}`)}}}
}

func TestAnthropicTranslationAllMessageShapes(t *testing.T) {
	system, got := buildAnthropicMessages(representativeMessages())
	if system != "first\n\n---\n\nsecond" || len(got) != 3 {
		t.Fatalf("system=%q messages=%#v", system, got)
	}
	if len(got[0].Content) != 2 || got[0].Content[1].Source == nil || got[0].Content[1].Source.MediaType != "image/png" {
		t.Fatalf("user image translation=%#v", got[0])
	}
	if len(got[1].Content) != 3 || string(got[1].Content[2].Input) != "{}" {
		t.Fatalf("assistant translation=%#v", got[1])
	}
	if got[2].Role != "user" || got[2].Content[0].Type != "tool_result" || got[2].Content[0].ToolUseID != "call-1" {
		t.Fatalf("tool result translation=%#v", got[2])
	}
	if buildAnthropicTools(nil) != nil {
		t.Fatal("empty tools should remain omitted")
	}
	if tools := buildAnthropicTools(representativeTools()); len(tools) != 1 || tools[0].Name != "lookup" {
		t.Fatalf("tools=%#v", tools)
	}
	for _, bad := range []string{"x", "data:x", "data:x;base64", "data:x,abc;base64", "data:x;base64,%%%"} {
		if parseDataURL(bad) != nil {
			t.Fatalf("accepted invalid data URL %q", bad)
		}
	}
}

func TestGeminiTranslationAllMessageShapes(t *testing.T) {
	system, got := buildGeminiContents(representativeMessages())
	if system == nil || system.Parts[0].Text != "first\n\n---\n\nsecond" || len(got) != 3 {
		t.Fatalf("system=%#v contents=%#v", system, got)
	}
	if got[0].Role != "user" || len(got[0].Parts) != 3 || got[0].Parts[1].InlineData == nil {
		t.Fatalf("user=%#v", got[0])
	}
	if got[1].Role != "model" || len(got[1].Parts) != 3 || string(got[1].Parts[2].FunctionCall.Args) != "{}" || got[1].Parts[1].ThoughtSignature != "opaque" {
		t.Fatalf("assistant=%#v", got[1])
	}
	if got[2].Parts[0].FunctionResp.Response["value"] != float64(42) {
		t.Fatalf("tool=%#v", got[2])
	}
	_, fallback := buildGeminiContents([]openai.ChatMessage{{Role: "tool", Name: "x", Content: "plain"}})
	if fallback[0].Parts[0].FunctionResp.Response["result"] != "plain" {
		t.Fatalf("fallback=%#v", fallback)
	}
	if buildGeminiTools(nil) != nil {
		t.Fatal("empty tools should remain omitted")
	}
	if tools := buildGeminiTools(representativeTools()); len(tools) != 1 || tools[0].FunctionDeclarations[0].Name != "lookup" {
		t.Fatalf("tools=%#v", tools)
	}
	for _, bad := range []string{"x", "data:x", "data:x;base64", "data:x,abc;base64"} {
		if _, _, ok := decodeDataURL(bad); ok {
			t.Fatalf("accepted invalid data URL %q", bad)
		}
	}
}

func TestResponsesTranslationAllMessageShapes(t *testing.T) {
	instructions, got := buildResponsesInput(representativeMessages())
	if instructions != "first\n\n---\n\nsecond" || len(got) != 5 {
		t.Fatalf("instructions=%q input=%#v", instructions, got)
	}
	if got[0].Type != "message" || got[0].Content[0].Type != "input_text" {
		t.Fatalf("user=%#v", got[0])
	}
	if got[1].Content[0].Type != "output_text" || got[2].Type != "function_call" || got[4].Type != "function_call_output" {
		t.Fatalf("input=%#v", got)
	}
	if buildResponsesTools(nil) != nil {
		t.Fatal("empty tools should remain omitted")
	}
	if tools := buildResponsesTools(representativeTools()); len(tools) != 1 || tools[0].Type != "function" || tools[0].Name != "lookup" {
		t.Fatalf("tools=%#v", tools)
	}
}

func TestProviderSmallMethods(t *testing.T) {
	caps := capabilities{images: true, tools: true}
	if !caps.SupportsImages() || !caps.SupportsTools() {
		t.Fatal("capabilities lost")
	}
	providers := []interface {
		ID() string
		Kind() string
	}{
		NewAnthropic("a", "", "key", "model", caps),
		NewGemini("g", "", "key", "model", caps),
		NewOpenAIResponses("r", "", nil, "model", caps),
	}
	for _, p := range providers {
		if p.ID() == "" || p.Kind() == "" {
			t.Fatalf("bad identity %#v", p)
		}
	}
	err := (&HTTPError{Provider: "p", Status: "503 Service Unavailable"}).Error()
	if err != "p: upstream returned 503 Service Unavailable" {
		t.Fatalf("error=%q", err)
	}
}
