package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codexmark/kram-gateway/internal/openai"
)

func TestChatCompletionSendsRunIDHeaderWhenSet(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(openai.RunIDHeader)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "hi"}}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := WithRunID(context.Background(), "run-A")
	if _, err := c.ChatCompletion(ctx, "default", nil, nil); err != nil {
		t.Fatal(err)
	}
	if gotHeader != "run-A" {
		t.Errorf("%s header = %q, want %q", openai.RunIDHeader, gotHeader, "run-A")
	}
}

func TestChatCompletionOmitsRunIDHeaderWhenUnset(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get(openai.RunIDHeader) != ""
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "hi"}}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	if _, err := c.ChatCompletion(context.Background(), "default", nil, nil); err != nil {
		t.Fatal(err)
	}
	if sawHeader {
		t.Error("a context with no run ID attached should not send the header at all")
	}
}

func TestChatCompletionStreamSendsRunIDHeaderWhenSet(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(openai.RunIDHeader)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := WithRunID(context.Background(), "run-B")
	deltas, err := c.ChatCompletionStream(ctx, "default", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range deltas {
	}
	if gotHeader != "run-B" {
		t.Errorf("%s header = %q, want %q", openai.RunIDHeader, gotHeader, "run-B")
	}
}
