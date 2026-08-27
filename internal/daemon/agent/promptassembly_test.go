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
	// Unit tests historically use small MaxTurns values as an exact hard
	// boundary. Keep them single-segment unless a segmentation test opts in.
	if cfg.MaxSegmentsPerRun == 0 {
		cfg.MaxSegmentsPerRun = 1
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tools.NewRegistry(workspace, st, nil)
	gw := gatewayclient.New(gatewayURL)
	svc, err := New(st, gw, tr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
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
	// [0..8] the nine named base sections (identity..safety, see
	// baseSectionOrder), [9] tools-overview, [10] background-job-guidance,
	// [11] AGENTS.md, [12] memory, [13] user turn.
	wantLen := len(baseSectionOrder) + 5
	if len(msgs) != wantLen {
		t.Fatalf("expected %d messages (%d base sections + tools-overview + background-job-guidance + AGENTS.md + memory + user turn), got %d: %+v", wantLen, len(baseSectionOrder), len(msgs), msgs)
	}
	if msgs[0].Role != "system" || msgs[0].Content == "" {
		t.Errorf("msgs[0] should be the non-empty identity section, got %+v", msgs[0])
	}
	toolsOverview := msgs[len(baseSectionOrder)]
	if toolsOverview.Role != "system" || !strings.Contains(toolsOverview.Content, "# Tools") || !strings.Contains(toolsOverview.Content, "run_background") {
		t.Errorf("msgs[%d] should be the generated tools overview (mentioning run_background, among every other registered tool), got %+v", len(baseSectionOrder), toolsOverview)
	}
	bgGuidance := msgs[len(baseSectionOrder)+1]
	if bgGuidance.Role != "system" || !strings.Contains(bgGuidance.Content, "run_background, not bash") {
		t.Errorf("msgs[%d] should be the background-job guidance (run_background is visible in this real registry), got %+v", len(baseSectionOrder)+1, bgGuidance)
	}
	projectContext := msgs[len(baseSectionOrder)+2]
	if projectContext.Role != "system" || !strings.Contains(projectContext.Content, "Always run tests before finishing.") {
		t.Errorf("msgs[%d] should be the AGENTS.md project-context message, got %+v", len(baseSectionOrder)+2, projectContext)
	}
	memory := msgs[len(baseSectionOrder)+3]
	if memory.Role != "system" || !strings.Contains(memory.Content, "the user prefers terse answers") {
		t.Errorf("msgs[%d] should be the memory message, got %+v", len(baseSectionOrder)+3, memory)
	}
	userTurn := msgs[len(baseSectionOrder)+4]
	if userTurn.Role != "user" || userTurn.Content != "oi" {
		t.Errorf("msgs[%d] should be the real conversation turn, got %+v", len(baseSectionOrder)+4, userTurn)
	}
}

// TestRunLoopPromptAssemblyEveryEnabledToolAppearsInPrompt is the
// regression test for the actual bug this registry exists to fix: 21 of
// 38 registered tools were once mentioned nowhere in the prompt, simply
// because nobody remembered to hand-add them (see DECISIONS.md). This
// runs a real Service.Run against the real tools.NewRegistry (not a
// mock) and asserts every tool AllTools() reports as enabled shows up
// somewhere in the generated system messages — the literal contract "a
// tool cannot silently go unmentioned again," proven against production
// tool wiring, not a hand-picked subset.
func TestRunLoopPromptAssemblyEveryEnabledToolAppearsInPrompt(t *testing.T) {
	workspace := t.TempDir()
	srv, requests := fakeGateway(t, []scriptedChatResponse{{content: "hello there"}})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "oi", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 gateway request, got %d", len(reqs))
	}
	var allContent strings.Builder
	for _, m := range reqs[0] {
		allContent.WriteString(m.Content)
		allContent.WriteString("\n")
	}
	prompt := allContent.String()

	for _, info := range s.tools.AllTools() {
		if info.Disabled {
			continue
		}
		if !strings.Contains(prompt, info.Name) {
			t.Errorf("enabled tool %q never appears anywhere in the generated prompt", info.Name)
		}
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

// TestRunLoopEmptyRetryNudgeClearsAfterProductiveTurn is the regression
// test for the latching bug: one empty response used to set emptyRetryUsed
// forever, so every later turn in the run — even ones mid-productive-tool-
// loop — kept getting the "your previous response was empty" nudge. Here
// the model goes empty, retries into a real tool call (a productive turn),
// then answers; the request that carries that final answer must NOT still
// carry the stale nudge.
func TestRunLoopEmptyRetryNudgeClearsAfterProductiveTurn(t *testing.T) {
	workspace := t.TempDir()
	toolCall := openai.ToolCall{
		ID: "call_process_list", Type: "function",
		Function: openai.ToolCallFunction{Name: "process_list", Arguments: `{}`},
	}
	srv, requests := fakeGateway(t, []scriptedChatResponse{
		{content: ""},                            // turn 1: empty -> triggers the nudge on the retry
		{toolCalls: []openai.ToolCall{toolCall}}, // turn 2 (retry): a real tool call — productive, clears the flag
		{content: "done"},                        // turn 3: final answer
	})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "oi", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := requests()
	if len(reqs) != 3 {
		t.Fatalf("expected exactly 3 gateway requests, got %d", len(reqs))
	}
	// The retry (request 2) legitimately carries the nudge...
	if r2 := reqs[1]; !strings.Contains(r2[len(r2)-1].Content, "was empty") {
		t.Errorf("request 2 (the retry) should carry the empty-retry nudge")
	}
	// ...but request 3, after a productive tool-call turn, must not.
	for _, m := range reqs[2] {
		if strings.Contains(m.Content, "was empty") {
			t.Fatalf("request 3 still carries the stale empty-retry nudge after a productive turn: %+v", m)
		}
	}
}
