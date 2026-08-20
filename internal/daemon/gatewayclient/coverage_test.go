package gatewayclient

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

func TestGatewayErrorString(t *testing.T) {
	if got := (&GatewayError{Message: "boom"}).Error(); got != "boom" {
		t.Fatalf("Error = %q", got)
	}
}

func TestChatCompletionResultAndProtocolFailures(t *testing.T) {
	want := openai.ChatCompletionResponse{
		Choices:  []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Content: "ok", ToolCalls: []openai.ToolCall{{ID: "call"}}}, FinishReason: "stop"}},
		Provider: "p1", Strategy: "round-robin", Attempts: []openai.AttemptInfo{{Provider: "p1"}}, Ranking: []openai.RankedProviderInfo{{Provider: "p1"}}, Usage: openai.Usage{TotalTokens: 3},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(want) }))
	defer srv.Close()
	got, err := New(srv.URL).ChatCompletion(context.Background(), "m", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || got.Provider != "p1" || got.Strategy != "round-robin" || len(got.ToolCalls) != 1 || len(got.Attempts) != 1 || len(got.Ranking) != 1 || got.Usage.TotalTokens != 3 {
		t.Fatalf("result = %+v", got)
	}

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"invalid error body", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418); _, _ = w.Write([]byte("nope")) }, "418"},
		{"bad success json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }, "decoding"},
		{"no choices", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }, "no choices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.handler)
			defer s.Close()
			_, err := New(s.URL).ChatCompletion(context.Background(), "m", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestClientTransportAndContextFailures(t *testing.T) {
	c := New("http://127.0.0.1:1")
	if _, err := c.ChatCompletion(context.Background(), "m", nil, nil); err == nil {
		t.Error("ChatCompletion transport succeeded")
	}
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("Status transport succeeded")
	}
	if _, err := c.ChatCompletionStream(context.Background(), "m", nil, nil); err == nil {
		t.Error("Stream transport succeeded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New("http://example.invalid").Status(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Status canceled err = %v", err)
	}
}

func TestStreamProtocolFailuresAndScannerLimit(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"structured", `{"error":{"message":"denied"}}`, "denied", http.StatusBadRequest},
		{"unstructured", `nope`, "418", http.StatusTeapot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			_, err := New(s.URL).ChatCompletionStream(context.Background(), "m", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + strings.Repeat("x", 5<<20) + "\n"))
	}))
	defer s.Close()
	deltas, err := New(s.URL).ChatCompletionStream(context.Background(), "m", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	for delta := range deltas {
		gotErr = delta.Err
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "reading gateway stream") {
		t.Fatalf("scanner err = %v", gotErr)
	}
}
