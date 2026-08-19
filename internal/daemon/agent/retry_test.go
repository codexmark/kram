package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/openai"
)

func TestBackoffWithJitterGrowsAndCaps(t *testing.T) {
	for round := 0; round < 6; round++ {
		got := backoffWithJitter(round, 0)
		if got <= 0 {
			t.Fatalf("round %d: backoff = %v, want positive", round, got)
		}
		if got > maxBackoff+maxBackoff/10 { // allow for jitter headroom above the cap's own -10%..+10% band
			t.Errorf("round %d: backoff = %v, exceeds maxBackoff %v beyond jitter tolerance", round, got, maxBackoff)
		}
	}
}

func TestBackoffWithJitterRespectsRetryAfterFloor(t *testing.T) {
	const floor = 9 * time.Second // deliberately larger than maxBackoff
	got := backoffWithJitter(0, floor)
	if got < floor {
		t.Errorf("backoff = %v, want at least the Retry-After floor of %v", got, floor)
	}
}

// gatewayErrorHandler returns an httptest handler that fails the first
// failCount requests with a writeGatewayError-shaped body (matching
// internal/server/chat.go's real wire format) carrying class/retryable,
// then succeeds.
func gatewayErrorHandler(t *testing.T, failCount int32, class openai.FailureClass, retryable bool) (*httptest.Server, *int32) {
	t.Helper()
	var seen int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&seen, 1)
		if n <= failCount {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(openai.ErrorResponse{Error: openai.ErrorBody{
				Message: "all providers in combo \"default\" failed", Type: "kram_gateway_error",
				Combo: "default", Retryable: retryable, Cause: class,
			}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "ok"}}},
		})
	}))
	return srv, &seen
}

// TestCallModelWithRetrySucceedsAfterRetryableFailures is the core
// regression test: a combo that fails with a retryable class (e.g. rate
// limit) on the first two rounds must succeed on the third, with a
// visible EventNotice per retry.
func TestCallModelWithRetrySucceedsAfterRetryableFailures(t *testing.T) {
	srv, seen := gatewayErrorHandler(t, 2, openai.ClassRateLimit, true)
	defer srv.Close()

	s := &Service{gateway: gatewayclient.New(srv.URL), heartbeatInterval: heartbeatInterval, cfg: Config{MaxGatewayRounds: 3}}

	var notices []string
	onEvent := func(evt Event) {
		if evt.Kind == EventNotice {
			notices = append(notices, evt.Notice)
		}
	}
	result, err := s.callModelWithRetry(context.Background(), "default", nil, nil, onEvent)
	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	if result.Content != "ok" {
		t.Errorf("Content = %q, want %q", result.Content, "ok")
	}
	if got := atomic.LoadInt32(seen); got != 3 {
		t.Errorf("gateway saw %d requests, want 3 (2 failures + 1 success)", got)
	}
	if len(notices) != 2 {
		t.Errorf("expected 2 retry notices, got %d: %v", len(notices), notices)
	}
}

// TestCallModelWithRetryStopsAfterNonRetryableFailure confirms a
// non-retryable class (e.g. Kram's own malformed request) fails fast —
// exactly one attempt, no retries, no wasted rounds.
func TestCallModelWithRetryStopsAfterNonRetryableFailure(t *testing.T) {
	srv, seen := gatewayErrorHandler(t, 99, openai.ClassInvalidRequest, false)
	defer srv.Close()

	s := &Service{gateway: gatewayclient.New(srv.URL), heartbeatInterval: heartbeatInterval, cfg: Config{MaxGatewayRounds: 3}}

	var notices int
	onEvent := func(evt Event) {
		if evt.Kind == EventNotice {
			notices++
		}
	}
	_, err := s.callModelWithRetry(context.Background(), "default", nil, nil, onEvent)
	if err == nil {
		t.Fatal("expected an error for a non-retryable failure, got success")
	}
	if got := atomic.LoadInt32(seen); got != 1 {
		t.Errorf("gateway saw %d requests, want exactly 1 (no retries for a non-retryable class)", got)
	}
	if notices != 0 {
		t.Errorf("expected 0 retry notices for a non-retryable failure, got %d", notices)
	}
}

// TestCallModelWithRetryGivesUpAfterMaxRounds confirms exhausting every
// round returns the last error instead of retrying forever.
func TestCallModelWithRetryGivesUpAfterMaxRounds(t *testing.T) {
	srv, seen := gatewayErrorHandler(t, 99, openai.ClassServerError, true)
	defer srv.Close()

	s := &Service{gateway: gatewayclient.New(srv.URL), heartbeatInterval: heartbeatInterval, cfg: Config{MaxGatewayRounds: 3}}

	_, err := s.callModelWithRetry(context.Background(), "default", nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error after exhausting every round")
	}
	if got := atomic.LoadInt32(seen); got != 3 {
		t.Errorf("gateway saw %d requests, want exactly MaxGatewayRounds=3", got)
	}
}

// TestCallModelWithRetryDoesNotRetryPlainNonGatewayError confirms a
// failure that never even reaches a GatewayError (e.g. the gateway
// itself is unreachable) fails immediately rather than retrying a
// problem retrying can't fix.
func TestCallModelWithRetryDoesNotRetryPlainNonGatewayError(t *testing.T) {
	s := &Service{gateway: gatewayclient.New("http://127.0.0.1:1"), heartbeatInterval: heartbeatInterval, cfg: Config{MaxGatewayRounds: 3}}

	_, err := s.callModelWithRetry(context.Background(), "default", nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error for an unreachable gateway")
	}
}
