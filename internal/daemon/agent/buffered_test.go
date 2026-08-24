package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/openai"
)

// TestBufferedCallEmitsHeartbeatDuringLongWait is the regression test for
// the CLI stall-warning risk: bufferedCall has no per-token output to
// relay while a buffered gateway call is in flight, so a heartbeat must
// fire periodically to keep a caller's own liveness clock fresh — see
// EventHeartbeat's doc comment.
func TestBufferedCallEmitsHeartbeatDuringLongWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond) // several heartbeat intervals below
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "hi"}}},
		})
	}))
	defer srv.Close()

	s := &Service{gateway: gatewayclient.New(srv.URL), heartbeatInterval: 20 * time.Millisecond}

	var heartbeats int
	onEvent := func(evt Event) {
		if evt.Kind == EventHeartbeat {
			heartbeats++
		}
	}
	result, err := s.bufferedCall(context.Background(), "default", nil, nil, onEvent)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "hi" {
		t.Errorf("Content = %q, want %q", result.Content, "hi")
	}
	if heartbeats < 2 {
		t.Errorf("expected multiple heartbeats during a 120ms wait with a 20ms interval, got %d", heartbeats)
	}
}

// TestBufferedCallFastResponseStillReturnsPromptly confirms the ticker
// doesn't delay a call that finishes before the first heartbeat would
// even fire.
func TestBufferedCallFastResponseStillReturnsPromptly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "fast"}}},
		})
	}))
	defer srv.Close()

	s := &Service{gateway: gatewayclient.New(srv.URL), heartbeatInterval: 1 * time.Hour}

	start := time.Now()
	result, err := s.bufferedCall(context.Background(), "default", nil, nil, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "fast" {
		t.Errorf("Content = %q, want %q", result.Content, "fast")
	}
	if elapsed > time.Second {
		t.Errorf("bufferedCall took %v for an instant response — the ticker shouldn't block the fast path", elapsed)
	}
}

// TestCallModelUsesStreamingWhenPreferStreamingSet confirms the escape
// hatch actually reaches the streaming path — a session that opts in
// must not silently stay on the buffered default.
func TestCallModelUsesStreamingWhenPreferStreamingSet(t *testing.T) {
	var sawStreamingRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		sawStreamingRequest = req.Stream
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := &Service{
		gateway: gatewayclient.New(srv.URL), heartbeatInterval: heartbeatInterval,
		cfg: Config{PreferStreaming: true},
	}
	if _, err := s.callModel(context.Background(), "default", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !sawStreamingRequest {
		t.Error("expected PreferStreaming to route through the streaming (stream:true) gateway call")
	}
}

// TestStreamCallEmitsReasoningEventSeparateFromDelta confirms a
// reasoning-carrying SSE chunk from the gateway reaches onEvent as a
// distinct EventReasoning, never folded into an EventDelta's Content —
// the property EventReasoning's own doc comment (and StreamEvent.
// Reasoning's, further downstream) depends on.
func TestStreamCallEmitsReasoningEventSeparateFromDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"weighing it up\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"final answer\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := &Service{gateway: gatewayclient.New(srv.URL), heartbeatInterval: heartbeatInterval, cfg: Config{PreferStreaming: true}}
	var events []Event
	if _, err := s.callModel(context.Background(), "default", nil, nil, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}

	var sawReasoning, sawDelta bool
	for _, e := range events {
		if e.Kind == EventReasoning {
			sawReasoning = true
			if e.Reasoning != "weighing it up" {
				t.Errorf("EventReasoning.Reasoning = %q, want %q", e.Reasoning, "weighing it up")
			}
			if e.Content != "" {
				t.Errorf("EventReasoning also carried Content = %q, want empty", e.Content)
			}
		}
		if e.Kind == EventDelta {
			sawDelta = true
			if e.Content != "final answer" {
				t.Errorf("EventDelta.Content = %q, want %q (reasoning must not leak in)", e.Content, "final answer")
			}
		}
	}
	if !sawReasoning {
		t.Fatalf("no EventReasoning emitted, got: %+v", events)
	}
	if !sawDelta {
		t.Fatalf("no EventDelta emitted, got: %+v", events)
	}
}

// TestCallModelUsesBufferedByDefault confirms the default (zero-value
// Config) path is buffered, not streaming.
func TestCallModelUsesBufferedByDefault(t *testing.T) {
	var sawStreamingRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		sawStreamingRequest = req.Stream
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer srv.Close()

	s := &Service{gateway: gatewayclient.New(srv.URL), heartbeatInterval: heartbeatInterval}
	if _, err := s.callModel(context.Background(), "default", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if sawStreamingRequest {
		t.Error("expected the default (PreferStreaming=false) path to send stream:false")
	}
}
