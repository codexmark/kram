package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestOpenAIResponsesHTTPErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
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
