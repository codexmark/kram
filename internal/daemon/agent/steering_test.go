package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

// TestSteeringAfterFinalAnswerKeepsTurnGoing (#110): a message queued
// while the model was writing its final answer must not be lost — the
// answer stands, the turn continues, and the next call sees both.
func TestSteeringAfterFinalAnswerKeepsTurnGoing(t *testing.T) {
	workspace := t.TempDir()
	var svc *Service
	var calls int32
	var secondCallMessages []openai.ChatMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := atomic.AddInt32(&calls, 1)
		content := "first answer"
		if n == 1 {
			// The user "types" while the model is answering: queue lands
			// before the answer is processed — the exact race steering
			// exists to win.
			svc.QueueSteering("sess-1", "also add tests")
		} else {
			secondCallMessages = req.Messages
			content = "second answer"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatMessage{Role: "assistant", Content: content}, FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	svc = newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 5})
	newTestSession(t, svc, "sess-1")

	var notices []string
	res, err := svc.Run(context.Background(), "sess-1", "build the thing", nil, func(e Event) {
		if e.Kind == EventNotice {
			notices = append(notices, e.Notice)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "second answer" {
		t.Fatalf("final = %q — the turn should have continued past the first answer", res.Message.Content)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}

	// The continuation call saw the finished first answer AND the queued
	// user message, in that order.
	var sawFirst, sawSteer bool
	for _, m := range secondCallMessages {
		if m.Role == "assistant" && m.Content == "first answer" {
			sawFirst = true
		}
		if m.Role == "user" && m.Content == "also add tests" {
			if !sawFirst {
				t.Fatal("steering message must come after the finished answer")
			}
			sawSteer = true
		}
	}
	if !sawFirst || !sawSteer {
		t.Fatalf("continuation call missing pieces: first=%v steer=%v", sawFirst, sawSteer)
	}

	var noticed bool
	for _, n := range notices {
		if strings.Contains(n, "queued message") {
			noticed = true
		}
	}
	if !noticed {
		t.Fatalf("expected a pickup notice, got %v", notices)
	}

	// Both messages persisted in order in the session.
	msgs, err := svc.store.ListMessages("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, m := range msgs {
		if m.Role == "user" || m.Role == "assistant" {
			seq = append(seq, m.Role+":"+m.Content)
		}
	}
	want := []string{"user:build the thing", "assistant:first answer", "user:also add tests", "assistant:second answer"}
	if len(seq) != len(want) {
		t.Fatalf("history = %v", seq)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("history[%d] = %q, want %q (full: %v)", i, seq[i], want[i], seq)
		}
	}
}

// TestLeftoverSteeringDrainsAtNextRunStart: content queued in the race
// window after a turn ended is picked up before the next run's first
// model call — never dropped.
func TestLeftoverSteeringDrainsAtNextRunStart(t *testing.T) {
	workspace := t.TempDir()
	srv, captured := fakeGateway(t, []scriptedChatResponse{{content: "ok"}})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 3})
	newTestSession(t, s, "sess-1")

	s.QueueSteering("sess-1", "leftover from the race window")
	if _, err := s.Run(context.Background(), "sess-1", "next prompt", nil, nil); err != nil {
		t.Fatal(err)
	}
	first := captured()[0]
	var saw bool
	for _, m := range first {
		if m.Role == "user" && m.Content == "leftover from the race window" {
			saw = true
		}
	}
	if !saw {
		t.Fatal("leftover steering never reached the model")
	}
}
