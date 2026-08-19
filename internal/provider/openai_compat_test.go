package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

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
