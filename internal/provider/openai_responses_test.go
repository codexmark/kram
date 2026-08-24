package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func TestOpenAIResponsesStreamsTextDeltas(t *testing.T) {
	srv := sseServer(t, []string{
		`{"type":"response.output_text.delta","delta":"Hel"}`,
		`{"type":"response.output_text.delta","delta":"lo"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":2}}}`,
	})
	defer srv.Close()

	p := NewOpenAIResponses("test", srv.URL, func(context.Context) (string, error) { return "tok", nil }, "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)

	var text string
	var final *StreamEvent
	for i := range got {
		text += got[i].Delta
		if got[i].Done {
			final = &got[i]
		}
	}
	if text != "Hello" {
		t.Errorf("assembled text = %q, want %q", text, "Hello")
	}
	if final == nil {
		t.Fatal("expected a final Done event")
	}
	if final.Usage == nil || final.Usage.PromptTokens != 10 || final.Usage.CompletionTokens != 2 || final.Usage.TotalTokens != 12 {
		t.Errorf("final.Usage = %+v, want prompt=10 completion=2 total=12", final.Usage)
	}
}

func TestOpenAIResponsesPreservesReasoningAndDetailedUsage(t *testing.T) {
	srv := sseServer(t, []string{
		`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"opaque","summary":[]}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":5},"output_tokens_details":{"reasoning_tokens":12}}}}`,
	})
	defer srv.Close()
	p := NewOpenAIResponses("test", srv.URL, func(context.Context) (string, error) { return "tok", nil }, "gpt-5.5", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	final := got[len(got)-1]
	if len(final.ProviderItems) != 1 || final.ProviderItems[0].EncryptedContent != "opaque" {
		t.Fatalf("provider items = %+v", final.ProviderItems)
	}
	if u := final.Usage; u == nil || u.CachedPromptTokens != 80 || u.CacheWritePromptTokens != 5 || u.ReasoningTokens != 12 || u.EstimatedCostMicros == 0 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestOpenAIResponsesRequestUsesCacheDeferredToolsAndReasoningReplay(t *testing.T) {
	var body responsesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\n"))
	}))
	defer srv.Close()
	p := NewOpenAIResponses("test", srv.URL, func(context.Context) (string, error) { return "tok", nil }, "gpt-5.5", capabilities{})
	req := openai.ChatCompletionRequest{
		PromptCacheKey: "kram-session", Tools: representativeTools(),
		Messages: []openai.ChatMessage{{Role: "assistant", ProviderItems: []openai.ProviderItem{{Type: "reasoning", ID: "rs_1", EncryptedContent: "opaque", Summary: json.RawMessage(`[]`)}}}},
	}
	events, err := p.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, events)
	if body.PromptCacheKey != "kram-session" || len(body.Include) != 1 {
		t.Fatalf("cache/include request = %+v", body)
	}
	if len(body.Input) != 1 || body.Input[0].Type != "reasoning" || body.Input[0].EncryptedContent != "opaque" {
		t.Fatalf("replayed input = %+v", body.Input)
	}
	if len(body.Tools) != 2 || !body.Tools[0].DeferLoading || body.Tools[1].Type != "tool_search" {
		t.Fatalf("tools = %+v", body.Tools)
	}
}

func TestOpenAIResponsesAssemblesFunctionCall(t *testing.T) {
	srv := sseServer(t, []string{
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"list_dir","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item":{"call_id":"call_1"},"delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.delta","item":{"call_id":"call_1"},"delta":"\"/tmp\"}"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3}}}`,
	})
	defer srv.Close()

	p := NewOpenAIResponses("test", srv.URL, func(context.Context) (string, error) { return "tok", nil }, "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)

	final := got[len(got)-1]
	if !final.Done {
		t.Fatalf("expected the last event to be Done, got %+v", final)
	}
	if len(final.ToolCalls) != 1 {
		t.Fatalf("expected exactly 1 assembled tool call, got %d: %+v", len(final.ToolCalls), final.ToolCalls)
	}
	tc := final.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "list_dir" {
		t.Errorf("tool call = %+v, want ID=call_1 Name=list_dir", tc)
	}
	if tc.Function.Arguments != `{"path":"/tmp"}` {
		t.Errorf("tool call arguments = %q, want assembled JSON", tc.Function.Arguments)
	}
}

func TestOpenAIResponsesJoinsRealCodexToolEventsByOutputIndex(t *testing.T) {
	srv := sseServer(t, []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"skill_list","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":0,"delta":"{}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_a","output_index":0,"arguments":"{}"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_b","type":"function_call","call_id":"call_b","name":"read_file","arguments":""}}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_b","output_index":1,"arguments":"{\"path\":\"README.md\"}"}`,
		`{"type":"response.completed","response":{"usage":{}}}`,
	})
	defer srv.Close()

	p := NewOpenAIResponses("test", srv.URL, func(context.Context) (string, error) { return "tok", nil }, "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	calls := got[len(got)-1].ToolCalls
	if len(calls) != 2 {
		t.Fatalf("tool calls = %+v, want exactly two and no phantom call", calls)
	}
	if calls[0].ID != "call_a" || calls[0].Function.Name != "skill_list" || calls[0].Function.Arguments != `{}` {
		t.Errorf("first call = %+v", calls[0])
	}
	if calls[1].ID != "call_b" || calls[1].Function.Arguments != `{"path":"README.md"}` {
		t.Errorf("second call = %+v", calls[1])
	}
}

func TestOpenAIResponsesHTTPErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("  invalid\nrequest  "))
	}))
	defer srv.Close()

	p := NewOpenAIResponses("test", srv.URL, func(context.Context) (string, error) { return "tok", nil }, "", capabilities{})
	_, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected an *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", httpErr.StatusCode, http.StatusUnauthorized)
	}
	if httpErr.Detail != "invalid request" || !strings.Contains(err.Error(), "invalid request") {
		t.Errorf("error detail = %q, error = %q", httpErr.Detail, err)
	}
}

func TestOpenAIResponsesPropagatesResolveError(t *testing.T) {
	resolveErr := errors.New("token expired, no refresh token on file")
	p := NewOpenAIResponses("test", "http://unused.invalid", func(context.Context) (string, error) { return "", resolveErr }, "", capabilities{})
	_, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err == nil || !errors.Is(err, resolveErr) {
		t.Errorf("err = %v, want it to wrap the resolve error", err)
	}
}

// TestOpenAIResponsesSendsBearerTokenFromResolve confirms the resolved
// credential is what actually goes on the wire, not something else —
// this adapter's whole reason for taking a resolve func instead of a
// static key.
func TestOpenAIResponsesSendsBearerTokenFromResolve(t *testing.T) {
	var gotAuth string
	var gotBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"type":"response.completed","response":{"usage":{}}}` + "\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAIResponses("test", srv.URL, func(context.Context) (string, error) { return "fresh-token", nil }, "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, events)

	if gotAuth != "Bearer fresh-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer fresh-token")
	}
	store, ok := gotBody["store"]
	if !ok {
		t.Fatal("request body omitted required store field")
	}
	if string(store) != "false" {
		t.Errorf("request body store = %s, want false", store)
	}
}

func TestBuildResponsesInputPullsSystemToInstructions(t *testing.T) {
	instructions, items := buildResponsesInput([]openai.ChatMessage{
		{Role: "system", Content: "be concise"},
		{Role: "user", Content: "hi"},
	})
	if instructions != "be concise" {
		t.Errorf("instructions = %q, want %q", instructions, "be concise")
	}
	if len(items) != 1 || items[0].Type != "message" || items[0].Role != "user" {
		t.Errorf("items = %+v, want one user message item", items)
	}
}

func TestBuildResponsesInputToolResultBecomesFunctionCallOutput(t *testing.T) {
	_, items := buildResponsesInput([]openai.ChatMessage{
		{Role: "tool", ToolCallID: "call_1", Content: "42"},
	})
	if len(items) != 1 || items[0].Type != "function_call_output" || items[0].CallID != "call_1" || items[0].Output != "42" {
		t.Errorf("items = %+v, want one function_call_output for call_1", items)
	}
}

func TestBuildResponsesInputAssistantToolCallBecomesFunctionCall(t *testing.T) {
	_, items := buildResponsesInput([]openai.ChatMessage{
		{Role: "assistant", ToolCalls: []openai.ToolCall{
			{ID: "call_1", Function: openai.ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`}},
		}},
	})
	if len(items) != 1 || items[0].Type != "function_call" || items[0].CallID != "call_1" || items[0].Name != "bash" {
		t.Errorf("items = %+v, want one function_call for call_1/bash", items)
	}
}
