package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

func TestOpenAICompatMergesSystemMessagesIntoOneLeadingMessage(t *testing.T) {
	var received openai.ChatCompletionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := NewOpenAICompatible("test", srv.URL, "", "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{Messages: []openai.ChatMessage{
		{Role: "system", Content: "base"},
		{Role: "system", Content: "tools"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "system", Content: "runtime reminder"},
		{Role: "user", Content: "second"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_ = collectEvents(t, events)

	if len(received.Messages) != 4 {
		t.Fatalf("messages = %+v, want one merged system plus three conversation messages", received.Messages)
	}
	if received.Messages[0].Role != "system" || received.Messages[0].Content != "base\n\ntools\n\nruntime reminder" {
		t.Fatalf("merged system message = %+v", received.Messages[0])
	}
	roles := []string{received.Messages[1].Role, received.Messages[2].Role, received.Messages[3].Role}
	if fmt.Sprint(roles) != "[user assistant user]" {
		t.Fatalf("non-system order changed: %v", roles)
	}
}

func TestOpenAICompatSurfacesSSEErrorEnvelope(t *testing.T) {
	srv := sseServer(t, []string{
		`{"error":{"message":"System message must be at the beginning"}}`,
	})
	defer srv.Close()

	p := NewOpenAICompatible("lmstudio", srv.URL, "", "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("expected exactly one stream error event, got %+v", got)
	}
	if want := "lmstudio: upstream stream error: System message must be at the beginning"; got[0].Err.Error() != want {
		t.Fatalf("stream error = %q, want %q", got[0].Err, want)
	}
}

// sseServer returns an httptest.Server that streams the given raw SSE
// "data:" lines verbatim, terminated by "[DONE]" — enough to drive
// OpenAICompatible.ChatCompletion without a real upstream.
func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
}

func collectEvents(t *testing.T, events <-chan StreamEvent) []StreamEvent {
	t.Helper()
	var got []StreamEvent
	for evt := range events {
		got = append(got, evt)
	}
	return got
}

// TestOpenAICompatCapturesOpenRouterReasoningField documents the
// already-working case: OpenRouter's wire extension names the field
// "reasoning".
func TestOpenAICompatCapturesOpenRouterReasoningField(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"reasoning":"thinking..."}}]}`,
		`{"choices":[{"delta":{"content":"hi"}}]}`,
	})
	defer srv.Close()

	p := NewOpenAICompatible("test", srv.URL, "", "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)

	if len(got) < 1 || got[0].Reasoning != "thinking..." {
		t.Fatalf("expected first event to carry the reasoning fragment, got %+v", got)
	}
}

// TestOpenAICompatCapturesReasoningContentField is the regression case:
// a real user-registered local server (vLLM-style reasoning parser) uses
// "reasoning_content" instead of OpenRouter's "reasoning" — before this
// fix that field went completely uncaptured, which made a genuinely
// working, actively-reasoning provider look like dead silence to
// router.BoundedPeek and get rejected as stalled.
func TestOpenAICompatCapturesReasoningContentField(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"reasoning_content":"The user wants..."}}]}`,
		`{"choices":[{"delta":{"content":"hi"}}]}`,
	})
	defer srv.Close()

	p := NewOpenAICompatible("test", srv.URL, "", "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)

	if len(got) < 1 || got[0].Reasoning != "The user wants..." {
		t.Fatalf("expected first event to carry the reasoning_content fragment, got %+v", got)
	}
}

// TestOpenAICompatEmitsToolCallProgress is the regression test for the
// confirmed liveness gap: a chunk carrying only tool-call argument
// fragments (no content, no reasoning) used to accumulate silently with
// no StreamEvent at all, leaving router.BoundedPeek with zero visibility
// into a provider that was actively streaming a tool call's arguments.
func TestOpenAICompatEmitsToolCallProgress(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_dir","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	defer srv.Close()

	p := NewOpenAICompatible("test", srv.URL, "", "", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)

	progressEvents := 0
	for _, e := range got {
		if e.ToolCallProgress {
			progressEvents++
		}
	}
	if progressEvents != 2 {
		t.Errorf("expected 2 ToolCallProgress events (one per tool-call-only chunk), got %d: %+v", progressEvents, got)
	}
}

// TestOpenAICompatParsesRetryAfterHeader confirms a real Retry-After
// header from a 429 response survives into the returned *HTTPError, so
// a Gateway Round retry can honor it instead of guessing a backoff.
func TestOpenAICompatParsesRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewOpenAICompatible("test", srv.URL, "", "", capabilities{})
	_, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected a *HTTPError, got %T: %v", err, err)
	}
	if httpErr.RetryAfter != 12*time.Second {
		t.Errorf("RetryAfter = %v, want 12s", httpErr.RetryAfter)
	}
}

// TestOpenAICompatMissingRetryAfterIsZero confirms the absent case
// degrades to zero (caller falls back to a computed backoff), not a
// parse panic or a bogus non-zero value.
func TestOpenAICompatMissingRetryAfterIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewOpenAICompatible("test", srv.URL, "", "", capabilities{})
	_, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected a *HTTPError, got %T: %v", err, err)
	}
	if httpErr.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 when the header is absent", httpErr.RetryAfter)
	}
}
