package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/openai"
)

// TestCompactionSummaryFailureFallsBackToEmergencyPrune (#114): when the
// summarizer's own model call fails, the turn must proceed on an
// emergency-pruned context instead of dying — the full history stays in
// the session, nothing is persisted for the failed summary, and the user
// sees an honest notice.
func TestCompactionSummaryFailureFallsBackToEmergencyPrune(t *testing.T) {
	var summaryCalls, chatCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "You are summarizing") {
			summaryCalls++
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"message":"summarizer down"}}`))
			return
		}
		chatCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatMessage{Role: "assistant", Content: "done"}, FinishReason: "stop",
			}},
		})
	}))
	defer srv.Close()

	workspace := t.TempDir()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 5, MaxContextTokens: 1500})
	newTestSession(t, s, "sess-1")

	// A history far over the 1500-token budget so the very first call
	// wants a compaction.
	big := strings.Repeat("x", 6000)
	for i := 0; i < 3; i++ {
		if _, err := s.store.AppendMessage("sess-1", store.Message{Role: "user", Content: "old question " + big}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.AppendMessage("sess-1", store.Message{Role: "assistant", Content: "old answer " + big}); err != nil {
			t.Fatal(err)
		}
	}

	var notices []string
	res, err := s.Run(context.Background(), "sess-1", "quick question", nil, func(e Event) {
		if e.Kind == EventNotice {
			notices = append(notices, e.Notice)
		}
	})
	if err != nil {
		t.Fatalf("turn must survive a failed summarizer, got: %v", err)
	}
	if res.Message.Content != "done" {
		t.Fatalf("final answer = %q", res.Message.Content)
	}
	if summaryCalls == 0 {
		t.Fatal("test setup: the summary call never fired")
	}
	if chatCalls == 0 {
		t.Fatal("the real model call never happened after the fallback")
	}

	var sawFallbackNotice bool
	for _, n := range notices {
		if strings.Contains(n, "summary model unavailable") {
			sawFallbackNotice = true
		}
	}
	if !sawFallbackNotice {
		t.Fatalf("expected the emergency-prune notice, got %v", notices)
	}

	// Nothing was persisted for the failed summary, and the old turns are
	// still in the session.
	msgs, err := s.store.ListMessages("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	oldTurns := 0
	for _, m := range msgs {
		if strings.Contains(m.Content, "old question") {
			oldTurns++
		}
		if m.Name == "kram:compaction_summary" {
			t.Fatal("a compaction marker was persisted despite the summarizer failing")
		}
	}
	if oldTurns != 3 {
		t.Fatalf("session lost history: %d old questions, want 3", oldTurns)
	}
}
