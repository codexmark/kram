package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/openai"
)

// midStreamDeathBody is a streaming gateway response that commits (real
// delta) and then dies with the typed terminal error chunk — carrying a
// retryable attempt class, exactly what kram-gateway writes when a
// committed upstream stream breaks (see internal/server/chat.go's
// writeFinal("error", ...)).
const midStreamDeathBody = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"error\"}],\"provider\":\"p1\",\"attempts\":[{\"provider\":\"p1\",\"class\":\"timeout\"}]}\n\n"

// TestStreamCallReturnsPartialAlongsideError: the salvage contract —
// whatever already streamed comes back with the error, so the retry can
// resume instead of regenerate.
func TestStreamCallReturnsPartialAlongsideError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(midStreamDeathBody))
	}))
	defer srv.Close()

	s := &Service{gateway: gatewayclient.New(srv.URL), heartbeatInterval: time.Hour, cfg: Config{PreferStreaming: true}}
	result, err := s.streamCall(context.Background(), "default", nil, nil, nil)
	if err == nil {
		t.Fatal("mid-stream death must surface an error")
	}
	var ge *gatewayclient.GatewayError
	if !asGatewayError(err, &ge) || !ge.Retryable {
		t.Fatalf("mid-stream death must be a retryable GatewayError, got %T: %v", err, err)
	}
	if result.Content != "Hello " {
		t.Fatalf("partial = %q, want %q", result.Content, "Hello ")
	}
}

func asGatewayError(err error, target **gatewayclient.GatewayError) bool {
	ge, ok := err.(*gatewayclient.GatewayError)
	if ok {
		*target = ge
	}
	return ok
}

// TestRetrySalvagesPartialAndResumes is the end-to-end regression test
// for #109: a stream that dies mid-answer retries with the partial fed
// back as a continuation, and the final result is partial+continuation —
// the turn succeeds instead of failing with "stream ended in error".
func TestRetrySalvagesPartialAndResumes(t *testing.T) {
	var calls int
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			w.Write([]byte(midStreamDeathBody))
			return
		}
		b := make([]byte, 1<<20)
		n, _ := r.Body.Read(b)
		secondBody = string(b[:n])
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := &Service{
		gateway: gatewayclient.New(srv.URL), heartbeatInterval: time.Hour,
		calibrator: newTokenCalibrator(),
		cfg:        Config{PreferStreaming: true, MaxGatewayRounds: 3},
	}

	var notices []string
	result, err := s.callModelWithRetry(context.Background(), "ses", 0, "default",
		[]openai.ChatMessage{{Role: "user", Content: "hi"}}, nil,
		func(e Event) {
			if e.Kind == EventNotice {
				notices = append(notices, e.Notice)
			}
		})
	if err != nil {
		t.Fatalf("salvage retry failed: %v", err)
	}
	if result.Content != "Hello world" {
		t.Fatalf("final content = %q, want partial+continuation %q", result.Content, "Hello world")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one failure, one resume)", calls)
	}

	// The retried call must carry the partial as the assistant's own words
	// plus the continuation directive — and neither may leak into the
	// session (only the request body sees them).
	var req openai.ChatCompletionRequest
	if err := json.Unmarshal([]byte(secondBody), &req); err != nil {
		t.Fatalf("second request body: %v", err)
	}
	last, prev := req.Messages[len(req.Messages)-1], req.Messages[len(req.Messages)-2]
	if prev.Role != "assistant" || prev.Content != "Hello " {
		t.Fatalf("resume call missing the partial as assistant message: %+v", prev)
	}
	if last.Role != "user" || !strings.Contains(last.Content, "cut off mid-stream") {
		t.Fatalf("resume call missing the continuation directive: %+v", last)
	}

	found := false
	for _, n := range notices {
		if strings.Contains(n, "resuming where it stopped") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a resuming notice, got %v", notices)
	}
}

// TestStreamingPreCommitGatewayErrorIsTyped: an all-candidates-failed
// round on the streaming path must reach the retry loop as a typed,
// retryable GatewayError — a flat string here silently disabled Gateway
// Rounds for every streaming session.
func TestStreamingPreCommitGatewayErrorIsTyped(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"message":"all failed","type":"kram_gateway_error","combo":"default","retryable":true,"attempts":[{"provider":"p1","class":"server_error"}]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := &Service{
		gateway: gatewayclient.New(srv.URL), heartbeatInterval: time.Hour,
		calibrator: newTokenCalibrator(),
		cfg:        Config{PreferStreaming: true, MaxGatewayRounds: 3},
	}
	result, err := s.callModelWithRetry(context.Background(), "ses", 0, "default",
		[]openai.ChatMessage{{Role: "user", Content: "hi"}}, nil, nil)
	if err != nil {
		t.Fatalf("pre-commit gateway error must be retried on the streaming path: %v", err)
	}
	if result.Content != "ok" || calls != 2 {
		t.Fatalf("content=%q calls=%d, want ok after one retry", result.Content, calls)
	}
}
