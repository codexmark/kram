package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
