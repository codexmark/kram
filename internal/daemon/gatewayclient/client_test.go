package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

func TestNewWithTimeout(t *testing.T) {
	c := NewWithTimeout("http://x", 7*time.Second)
	if c.http.Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want 7s", c.http.Timeout)
	}
	// A non-positive timeout falls back to the default.
	if got := NewWithTimeout("http://x", 0).http.Timeout; got != DefaultTimeout {
		t.Errorf("timeout with 0 = %v, want DefaultTimeout %v", got, DefaultTimeout)
	}
	if got := NewWithTimeout("http://x", -5).http.Timeout; got != DefaultTimeout {
		t.Errorf("timeout with negative = %v, want DefaultTimeout %v", got, DefaultTimeout)
	}
	// New keeps its historical default.
	if got := New("http://x").http.Timeout; got != DefaultTimeout {
		t.Errorf("New timeout = %v, want DefaultTimeout %v", got, DefaultTimeout)
	}
}

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

func TestChatCompletionSendsStablePromptCacheKey(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(openai.PromptCacheKeyHeader)
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant"}}}})
	}))
	defer srv.Close()
	ctx := WithPromptCacheKey(context.Background(), "kram-session")
	if _, err := New(srv.URL).ChatCompletion(ctx, "default", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got != "kram-session" {
		t.Fatalf("cache key = %q", got)
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

// TestChatCompletionDecodesStructuredGatewayError is the regression test
// for the old flat-error-only behavior: a kram-gateway all-failed
// response (Combo non-empty, the origin signal — see
// internal/server/chat.go's writeGatewayError) must decode into a typed
// *GatewayError a caller can inspect, not just a generic error string.
func TestChatCompletionDecodesStructuredGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(openai.ErrorResponse{Error: openai.ErrorBody{
			Message: "all providers in combo \"default\" failed, last error: p1: upstream returned 429 Too Many Requests",
			Type:    "kram_gateway_error",
			Combo:   "default", Retryable: true, RetryAfterMS: 5000, Cause: openai.ClassRateLimit,
			Attempts: []openai.AttemptInfo{{Provider: "p1", HTTPStatus: 429, Class: openai.ClassRateLimit}},
		}})
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ChatCompletion(context.Background(), "default", nil, nil)

	var ge *GatewayError
	if !errors.As(err, &ge) {
		t.Fatalf("expected a *GatewayError, got %T: %v", err, err)
	}
	if ge.Combo != "default" {
		t.Errorf("Combo = %q, want %q", ge.Combo, "default")
	}
	if !ge.Retryable {
		t.Error("Retryable = false, want true")
	}
	if ge.RetryAfter != 5*time.Second {
		t.Errorf("RetryAfter = %v, want 5s", ge.RetryAfter)
	}
	if ge.Cause != openai.ClassRateLimit {
		t.Errorf("Cause = %q, want %q", ge.Cause, openai.ClassRateLimit)
	}
	if len(ge.Attempts) != 1 || ge.Attempts[0].Provider != "p1" {
		t.Errorf("Attempts = %+v, want one entry for p1", ge.Attempts)
	}
}

// TestChatCompletionFallsBackToPlainErrorForNonKramServer confirms
// ChatCompletion still works against any OpenAI-compatible server's
// plain error body (no Combo field at all) — the flat-error path from
// before this change must survive unchanged for non-Kram endpoints.
func TestChatCompletionFallsBackToPlainErrorForNonKramServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(openai.ErrorResponse{Error: openai.ErrorBody{
			Message: "invalid request: missing required field 'model'",
			Type:    "invalid_request_error",
		}})
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ChatCompletion(context.Background(), "default", nil, nil)

	var ge *GatewayError
	if errors.As(err, &ge) {
		t.Fatalf("expected a plain error for a non-Kram error body, got a *GatewayError: %+v", ge)
	}
	if err == nil || !strings.Contains(err.Error(), "missing required field") {
		t.Errorf("err = %v, want it to mention the upstream message", err)
	}
}
