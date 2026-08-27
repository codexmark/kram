package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/daemon/tools"
	"github.com/codexmark/kram/internal/openai"
)

func coverageService(t *testing.T) *Service {
	t.Helper()
	workspace := t.TempDir()
	st, err := store.Open(filepath.Join(workspace, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := New(st, gatewayclient.New("http://127.0.0.1:1"), tools.NewRegistry(workspace, st, nil), Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestConfigDefaultsAndServiceRegistryPassthrough(t *testing.T) {
	c := (Config{}).withDefaults()
	if c.MaxTurns != 50 || c.MaxSegmentsPerRun != 4 || c.MaxCompactionsPerRun != 3 || c.MaxContextTokens == 0 || c.MaxGatewayRounds == 0 {
		t.Fatalf("defaults = %+v", c)
	}
	s := coverageService(t)
	if len(s.Tools()) == 0 {
		t.Fatal("Tools empty")
	}
	_ = s.Skills()
	s.ReplaceDisabledTools([]string{"read_file"})
	for _, info := range s.Tools() {
		if info.Name == "read_file" && !info.Disabled {
			t.Fatal("read_file remained enabled")
		}
	}
}

func TestAnswerQuestionAndApprovalChannels(t *testing.T) {
	s := coverageService(t)
	q := make(chan string, 1)
	s.pending["q"] = q
	if !s.AnswerQuestion("q", "yes") || <-q != "yes" {
		t.Fatal("question not delivered")
	}
	if s.AnswerQuestion("missing", "x") {
		t.Fatal("unknown question accepted")
	}
	// Full channel covers the non-blocking default branch.
	q <- "occupied"
	if !s.AnswerQuestion("q", "ignored") || <-q != "occupied" {
		t.Fatal("full question channel changed")
	}

	a := make(chan tools.ApprovalDecision, 1)
	s.pendingApprovals["a"] = a
	if !s.AnswerApproval("a", "once") || <-a != tools.ApprovalOnce {
		t.Fatal("approval not delivered")
	}
	if s.AnswerApproval("a", "invalid") || s.AnswerApproval("missing", "deny") {
		t.Fatal("invalid approval accepted")
	}
	a <- tools.ApprovalDeny
	if !s.AnswerApproval("a", "always") || <-a != tools.ApprovalDeny {
		t.Fatal("full approval channel changed")
	}
}

func TestSessionAskerAndApproverDeliverAndCancel(t *testing.T) {
	s := coverageService(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ask := &sessionAsker{svc: s, onEvent: func(e Event) {
		if !s.AnswerQuestion(e.QuestionID, "answer") {
			t.Error("answer rejected")
		}
	}}
	got, err := ask.Ask(ctx, "question", []string{"answer"})
	if err != nil || got != "answer" {
		t.Fatalf("Ask = %q, %v", got, err)
	}
	approve := &sessionApprover{svc: s, onEvent: func(e Event) {
		if !s.AnswerApproval(e.ApprovalID, "always") {
			t.Error("approval rejected")
		}
	}}
	decision, err := approve.Approve(ctx, "bash", "echo", "")
	if err != nil || decision != tools.ApprovalAlways {
		t.Fatalf("Approve = %q, %v", decision, err)
	}

	// A non-nil onEvent (a real interactive session) that never answers,
	// with an already-canceled context, must surface the cancellation —
	// exercises the ctx.Done() branch of the select. The onEvent is a
	// no-op here specifically because it must be non-nil to reach that
	// path at all (a nil onEvent short-circuits first — see below).
	noop := func(Event) {}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := (&sessionAsker{svc: s, onEvent: noop}).Ask(canceled, "q", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Ask err = %v", err)
	}
	if decision, err := (&sessionApprover{svc: s, onEvent: noop}).Approve(canceled, "bash", "x", ""); !errors.Is(err, context.Canceled) || decision != tools.ApprovalDeny {
		t.Fatalf("canceled Approve = %q, %v", decision, err)
	}
}

// TestSessionAskerApproverNilOnEventShortCircuits pins the subagent
// safety fix: with no live event sink (onEvent == nil, the RunTask case),
// Ask and Approve must return immediately rather than block for the full
// 10-minute timeout emitting a prompt nobody can see. Approve denies (a
// subagent must never auto-approve); Ask fails with a clear reason.
func TestSessionAskerApproverNilOnEventShortCircuits(t *testing.T) {
	s := coverageService(t)
	// A plain background context (not canceled, no deadline): if the
	// short-circuit is missing, this test hangs for approvalTimeout
	// instead of failing fast — the exact stall the fix prevents.
	ctx := context.Background()

	decision, err := (&sessionApprover{svc: s}).Approve(ctx, "bash", "rm -rf /", "")
	if decision != tools.ApprovalDeny || err != nil {
		t.Fatalf("nil-onEvent Approve = %q, %v; want ApprovalDeny, nil", decision, err)
	}

	if _, err := (&sessionAsker{svc: s}).Ask(ctx, "which option?", []string{"a", "b"}); err == nil {
		t.Fatal("nil-onEvent Ask should return an error, not block or succeed")
	}
}

func TestContextUsageAndRunFailures(t *testing.T) {
	s := coverageService(t)
	if _, err := s.ContextUsage(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ContextUsage missing = %v", err)
	}
	if _, err := s.Run(context.Background(), "missing", "hi", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Run missing = %v", err)
	}
	if _, err := s.store.CreateSession("s", "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AppendMessage("s", store.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	usage, err := s.ContextUsage(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Budget == 0 || usage.Used == 0 || usage.Free < 0 || len(usage.Categories) < 5 {
		t.Fatalf("usage = %+v", usage)
	}
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ContextUsage(context.Background(), "s"); err == nil {
		t.Fatal("ContextUsage on closed store succeeded")
	}
	if _, err := s.Run(context.Background(), "s", "hi", nil, nil); err == nil {
		t.Fatal("Run on closed store succeeded")
	}
}

// TestContextUsageSystemPromptCategoryIsNonZero is the regression test
// for a real bug the Model/Agent Profile phase's split introduced and
// caught along the way: context_usage.go used to key its "system_prompt"
// category directly off partTokens["base"], which only exists when
// Config.SystemPromptOverride is set — in the default (no override) case,
// compilePreamble now returns nine separately-IDed base sections instead
// of one "base" part, so that lookup would have silently reported 0
// tokens for the system prompt on every session that doesn't use an
// override, which is the common case.
func TestContextUsageSystemPromptCategoryIsNonZero(t *testing.T) {
	s := coverageService(t)
	if _, err := s.store.CreateSession("sp", "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AppendMessage("sp", store.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	usage, err := s.ContextUsage(context.Background(), "sp")
	if err != nil {
		t.Fatal(err)
	}
	for _, cat := range usage.Categories {
		if cat.Name == "system_prompt" {
			if cat.Tokens <= 0 {
				t.Fatalf("system_prompt category = %d tokens, want > 0: %+v", cat.Tokens, usage.Categories)
			}
			return
		}
	}
	t.Fatalf("no system_prompt category in %+v", usage.Categories)
}

func TestRunTaskCreatesIsolatedSessionAndUsesModel(t *testing.T) {
	gw, requests := fakeGateway(t, []scriptedChatResponse{{content: "child answer"}})
	defer gw.Close()
	workspace := t.TempDir()
	s := newTestService(t, workspace, gw.URL, Config{Workspace: workspace, Model: "parent", MaxTurns: 2})
	got, err := s.RunTask(context.Background(), "do it", "facts", "child", 1)
	if err != nil || got != "child answer" {
		t.Fatalf("RunTask = %q, %v", got, err)
	}
	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if last := reqs[0][len(reqs[0])-1].Content; last != "do it\n\nContext:\nfacts" {
		t.Fatalf("child prompt = %q", last)
	}
	if got, err := s.RunTask(context.Background(), "use parent", "", "", 1); err != nil || got != "child answer" {
		t.Fatalf("RunTask parent model = %q, %v", got, err)
	}

	closed := coverageService(t)
	_ = closed.store.Close()
	if _, err := closed.RunTask(context.Background(), "x", "", "", 0); err == nil {
		t.Fatal("RunTask on closed store succeeded")
	}
}

// TestRunTaskSessionIsSubagentTitledAndFetchableByID confirms the two
// facts issue #31's CLI-side picker filter (internal/cli/app/picker.go)
// depends on, without any daemon-side change: RunTask's session is
// discoverable by its "subagent: " title prefix (the same convention
// store/search.go's own isSubagentTitle already uses), and its messages
// are readable by session id through the same store methods any ordinary
// session already uses — no special-casing needed for a subagent session
// to be fetchable, live or after the fact.
func TestRunTaskSessionIsSubagentTitledAndFetchableByID(t *testing.T) {
	gw, _ := fakeGateway(t, []scriptedChatResponse{{content: "child answer"}})
	defer gw.Close()
	workspace := t.TempDir()
	s := newTestService(t, workspace, gw.URL, Config{Workspace: workspace, Model: "parent", MaxTurns: 2})

	before, err := s.store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions before RunTask: %v", err)
	}
	if _, err := s.RunTask(context.Background(), "investigate the flaky test", "", "child", 1); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	after, err := s.store.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions after RunTask: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("session count = %d, want %d (+1 for the subagent session)", len(after), len(before)+1)
	}
	var subagentID string
	for _, sess := range after {
		if strings.HasPrefix(sess.Title, "subagent: ") {
			subagentID = sess.ID
		}
	}
	if subagentID == "" {
		t.Fatalf("no session titled with the \"subagent: \" prefix among: %+v", after)
	}

	fetched, err := s.store.GetSession(subagentID)
	if err != nil {
		t.Fatalf("GetSession(%q): %v", subagentID, err)
	}
	if !strings.Contains(fetched.Title, "investigate the flaky test") {
		t.Fatalf("fetched session title = %q, want it to contain the goal", fetched.Title)
	}
	msgs, err := s.store.ListMessages(subagentID)
	if err != nil {
		t.Fatalf("ListMessages(%q): %v", subagentID, err)
	}
	if len(msgs) == 0 {
		t.Fatal("subagent session has no messages, want at least the delegated goal's turn")
	}
}

func TestRunToolAndImageCapability(t *testing.T) {
	s := coverageService(t)
	path := filepath.Join(s.cfg.Workspace, "large.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	a, m := s.runTool(context.Background(), openai.ToolCall{ID: "id", Function: openai.ToolCallFunction{Name: "read_file", Arguments: `{"path":"large.txt"}`}})
	if !a.OK || a.Result != "hello" || m.Content != "hello" || m.ToolCallID != "id" {
		t.Fatalf("activity=%+v message=%+v", a, m)
	}
	if err := os.WriteFile(path, []byte(string(make([]byte, maxToolResultChars+100))), 0644); err != nil {
		t.Fatal(err)
	}
	a, _ = s.runTool(context.Background(), openai.ToolCall{Function: openai.ToolCallFunction{Name: "read_file", Arguments: `{"path":"large.txt"}`}})
	if len(a.Result) != maxToolResultChars+len("…") {
		t.Fatalf("display length = %d", len(a.Result))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/status" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(gatewayclient.Status{Providers: []gatewayclient.ProviderStatus{{ID: "vision", SupportsImages: true}}, Combos: []gatewayclient.ComboStatus{{ID: "combo", Providers: []string{"vision"}}}})
	}))
	defer server.Close()
	s.gateway = gatewayclient.New(server.URL)
	s.cfg.Model = "combo"
	ok, err := s.comboSupportsImages(context.Background())
	if err != nil || !ok {
		t.Fatalf("comboSupportsImages = %v, %v", ok, err)
	}
}

func TestStreamCallSuccessAndGatewayError(t *testing.T) {
	finish := "stop"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := openai.ChatCompletionChunk{Choices: []openai.ChatCompletionChunkChoice{{Delta: openai.ChatCompletionChunkDelta{Content: "hello"}}}}
		b, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		chunk = openai.ChatCompletionChunk{Provider: "p", Choices: []openai.ChatCompletionChunkChoice{{FinishReason: &finish}}}
		b, _ = json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
	}))
	defer server.Close()
	s := coverageService(t)
	s.gateway = gatewayclient.New(server.URL)
	var deltas []string
	got, err := s.streamCall(context.Background(), "m", nil, nil, func(e Event) {
		if e.Kind == EventDelta {
			deltas = append(deltas, e.Content)
		}
	})
	if err != nil || got.Content != "hello" || got.Provider != "p" || len(deltas) != 1 {
		t.Fatalf("stream = %+v, %v, deltas=%v", got, err, deltas)
	}

	s.gateway = gatewayclient.New("http://127.0.0.1:1")
	if _, err := s.streamCall(context.Background(), "m", nil, nil, nil); err == nil {
		t.Fatal("stream transport succeeded")
	}
}

func TestRunRetriesEmptyResponseThenPersistsFallback(t *testing.T) {
	gw, _ := fakeGateway(t, []scriptedChatResponse{{content: ""}, {content: ""}})
	defer gw.Close()
	workspace := t.TempDir()
	s := newTestService(t, workspace, gw.URL, Config{Workspace: workspace, MaxTurns: 3})
	newTestSession(t, s, "empty")
	got, err := s.Run(context.Background(), "empty", "hello", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Message.Content == "" || !strings.Contains(got.Message.Content, "no response") {
		t.Fatalf("fallback = %q", got.Message.Content)
	}
}

func TestRunDropsUnsupportedImagesAndFailsOpenStatusError(t *testing.T) {
	var chatRequests []openai.ChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/status" {
			_ = json.NewEncoder(w).Encode(gatewayclient.Status{Combos: []gatewayclient.ComboStatus{{ID: "combo", Providers: []string{"text"}}}, Providers: []gatewayclient.ProviderStatus{{ID: "text"}}})
			return
		}
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		chatRequests = append(chatRequests, req)
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Content: "ok"}}}})
	}))
	defer server.Close()
	workspace := t.TempDir()
	s := newTestService(t, workspace, server.URL, Config{Workspace: workspace, Model: "combo", MaxTurns: 2})
	newTestSession(t, s, "img")
	var notices []Event
	got, err := s.Run(context.Background(), "img", "look", []string{"data:x"}, func(e Event) { notices = append(notices, e) })
	if err != nil || got.ImageNotice == "" || len(notices) == 0 || len(chatRequests) != 1 {
		t.Fatalf("run=%+v err=%v notices=%v requests=%d", got, err, notices, len(chatRequests))
	}
	msgs, _ := s.store.ListMessages("img")
	if len(msgs) == 0 || len(msgs[0].Images) != 0 {
		t.Fatalf("stored images = %+v", msgs)
	}

	// A failed capability probe deliberately fails open and retains images.
	s.gateway = gatewayclient.New("http://127.0.0.1:1")
	newTestSession(t, s, "fail-open")
	_, err = s.Run(context.Background(), "fail-open", "look", []string{"data:x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "gateway call failed") {
		t.Fatalf("fail-open call err=%v", err)
	}
	msgs, _ = s.store.ListMessages("fail-open")
	if len(msgs) == 0 || len(msgs[0].Images) != 1 {
		t.Fatalf("fail-open stored images = %+v", msgs)
	}
}

func TestRunCompactsAndStopsAfterConfiguredOverflowLimit(t *testing.T) {
	gw, _ := fakeGateway(t, []scriptedChatResponse{{content: "summary"}})
	defer gw.Close()
	workspace := t.TempDir()
	s := newTestService(t, workspace, gw.URL, Config{Workspace: workspace, MaxTurns: 5, MaxContextTokens: 1, MaxCompactionsPerRun: 1})
	newTestSession(t, s, "compact")
	var notices []Event
	_, err := s.Run(context.Background(), "compact", strings.Repeat("history ", 100), nil, func(e Event) {
		if e.Kind == EventNotice {
			notices = append(notices, e)
		}
	})
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("err=%v want ErrContextOverflow", err)
	}
	if len(notices) != 1 || !strings.Contains(notices[0].Notice, "compacted") {
		t.Fatalf("notices=%+v", notices)
	}
	msgs, listErr := s.store.ListMessages("compact")
	if listErr != nil {
		t.Fatal(listErr)
	}
	found := false
	for _, m := range msgs {
		if m.Name != "" && m.Role == "system" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no compaction marker in %+v", msgs)
	}
}
