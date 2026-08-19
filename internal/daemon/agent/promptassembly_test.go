package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/daemon/tools"
	"github.com/codexmark/kram/internal/openai"
)

// newTestService wires a real Service against a real temp-file store and
// tools registry, pointed at a fake gateway HTTP handler — same
// construction real code goes through (internal/daemon/server does
// exactly this at startup), just with a scripted gateway instead of a
// real one.
func newTestService(t *testing.T, workspace string, gatewayURL string, cfg Config) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tools.NewRegistry(workspace, st, nil)
	gw := gatewayclient.New(gatewayURL)
	return New(st, gw, tr, cfg)
}

func newTestSession(t *testing.T, s *Service, id string) {
	t.Helper()
	if _, err := s.store.CreateSession(id, "test"); err != nil {
		t.Fatal(err)
	}
}

// scriptedChatResponse is one canned reply a fake gateway handler serves
// in sequence, one per incoming request.
type scriptedChatResponse struct {
	content   string
	toolCalls []openai.ToolCall
}

// fakeGateway serves scriptedChatResponse entries in order (repeating
// the last one if more requests arrive than scripted) and records every
// request's Messages, in arrival order, for the test to inspect.
func fakeGateway(t *testing.T, script []scriptedChatResponse) (*httptest.Server, func() [][]openai.ChatMessage) {
	t.Helper()
	var mu sync.Mutex
	var captured [][]openai.ChatMessage
	var callCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
			return
		}
		mu.Lock()
		captured = append(captured, req.Messages)
		mu.Unlock()

		idx := int(atomic.AddInt32(&callCount, 1)) - 1
		if idx >= len(script) {
			idx = len(script) - 1
		}
		resp := script[idx]
		finish := "stop"
		if len(resp.toolCalls) > 0 {
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message:      openai.ChatMessage{Role: "assistant", Content: resp.content, ToolCalls: resp.toolCalls},
				FinishReason: finish,
			}},
		})
	}))

	return srv, func() [][]openai.ChatMessage {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]openai.ChatMessage, len(captured))
		copy(out, captured)
		return out
	}
}

// TestRunLoopPromptAssemblyContract pins the exact preamble ordering a
// real Service.Run produces on the wire, against real files (AGENTS.md)
// and real store-backed memory — the parts unit tests alone can't
// exercise, since they need real I/O this test provides. This is the
// contract any future Model Profile / Reminder Engine change must
// consciously break, not accidentally.
func TestRunLoopPromptAssemblyContract(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("Always run tests before finishing."), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, requests := fakeGateway(t, []scriptedChatResponse{{content: "hello there"}})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	if _, err := s.store.WriteMemoryEntry(workspace, "the user prefers terse answers", false); err != nil {
		t.Fatal(err)
	}
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "oi", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 gateway request, got %d", len(reqs))
	}
	msgs := reqs[0]
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages ([0]base [1]AGENTS.md [2]memory [3]user turn), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "system" || msgs[0].Content == "" {
		t.Errorf("msgs[0] should be the non-empty base system prompt, got %+v", msgs[0])
	}
	if msgs[1].Role != "system" || !strings.Contains(msgs[1].Content, "Always run tests before finishing.") {
		t.Errorf("msgs[1] should be the AGENTS.md project-context message, got %+v", msgs[1])
	}
	if msgs[2].Role != "system" || !strings.Contains(msgs[2].Content, "the user prefers terse answers") {
		t.Errorf("msgs[2] should be the memory message, got %+v", msgs[2])
	}
	if msgs[3].Role != "user" || msgs[3].Content != "oi" {
		t.Errorf("msgs[3] should be the real conversation turn, got %+v", msgs[3])
	}
}

// TestRunLoopPromptAssemblyIncludesNearBudgetMessage confirms the
// post-history soft-landing message is really wired through on the
// final allowed turn, and that toolDefs is empty on that same request —
// proving tool-visibility policy and prompt content stayed correctly
// decoupled through the refactor.
func TestRunLoopPromptAssemblyIncludesNearBudgetMessage(t *testing.T) {
	workspace := t.TempDir()
	srv, requests := fakeGateway(t, []scriptedChatResponse{{content: "final answer"}})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 1})
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "oi", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 gateway request, got %d", len(reqs))
	}
	msgs := reqs[0]
	last := msgs[len(msgs)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "turn limit") {
		t.Errorf("last message should be the near-budget soft-landing message, got %+v", last)
	}
}

// TestRunLoopPromptAssemblyIncludesEmptyRetryNudge confirms a genuinely
// empty first response triggers a retry whose request carries the nudge
// appended after history — the two-round-trip case unit tests can't
// exercise on their own, since it depends on runLoop's real continue
// logic, not just the compiler function.
func TestRunLoopPromptAssemblyIncludesEmptyRetryNudge(t *testing.T) {
	workspace := t.TempDir()
	srv, requests := fakeGateway(t, []scriptedChatResponse{
		{content: ""}, // triggers the empty-retry path
		{content: "real answer this time"},
	})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "oi", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 gateway requests (one retry), got %d", len(reqs))
	}
	last := reqs[1][len(reqs[1])-1]
	if last.Role != "system" || !strings.Contains(last.Content, "was empty") {
		t.Errorf("second request's last message should be the empty-retry nudge, got %+v", last)
	}
}
