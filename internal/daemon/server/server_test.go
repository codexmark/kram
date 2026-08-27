package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/daemon/agent"
	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/daemon/session"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/daemon/tools"
	"github.com/codexmark/kram/internal/openai"
)

// newTestServer wires a real Server against a real temp-file store,
// session service and agent service, pointed at a scripted fake gateway
// — the same construction daemon.Run does, just with a fake upstream.
// Mirrors internal/daemon/agent/promptassembly_test.go's newTestService.
func newTestServer(t *testing.T, script []openai.ChatCompletionResponse) *Server {
	srv, _ := newTestServerWithRegistry(t, script)
	return srv
}

func newTestServerWithRegistry(t *testing.T, script []openai.ChatCompletionResponse) (*Server, *tools.Registry) {
	t.Helper()
	workspace := t.TempDir()
	st, err := store.Open(filepath.Join(workspace, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tools.NewRegistry(workspace, st, nil)

	var idx int
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := script[idx]
		if idx < len(script)-1 {
			idx++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(gwSrv.Close)

	gw := gatewayclient.New(gwSrv.URL)
	agentSvc, err := agent.New(st, gw, tr, agent.Config{Workspace: workspace, MaxTurns: 10})
	if err != nil {
		t.Fatal(err)
	}
	sessSvc := session.New(st)

	return New(sessSvc, agentSvc, nil, ""), tr
}

// newStreamingTestServer wires a real Server against a real temp-file
// store, backed by a fake gateway that speaks real SSE (unlike
// newTestServer's plain-JSON fake, it honors req.Stream) — the setup
// TestHandleSendMessageRelaysReasoningEventOverSSE needs, since
// PreferStreaming only ever produces per-fragment events (reasoning
// included) on the streaming gateway path (see streamCall).
func newStreamingTestServer(t *testing.T, sseBody string) *Server {
	t.Helper()
	workspace := t.TempDir()
	st, err := store.Open(filepath.Join(workspace, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tools.NewRegistry(workspace, st, nil)

	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseBody)
	}))
	t.Cleanup(gwSrv.Close)

	gw := gatewayclient.New(gwSrv.URL)
	agentSvc, err := agent.New(st, gw, tr, agent.Config{Workspace: workspace, MaxTurns: 10, PreferStreaming: true})
	if err != nil {
		t.Fatal(err)
	}
	sessSvc := session.New(st)
	return New(sessSvc, agentSvc, nil, "")
}

// TestHandleSendMessageRelaysReasoningEventOverSSE confirms
// agent.EventReasoning — emitted by streamCall when Config.PreferStreaming
// is set — reaches the daemon's own SSE stream as {"type":"reasoning",
// "content":...}, the last hop before internal/cli/daemonclient.
func TestHandleSendMessageRelaysReasoningEventOverSSE(t *testing.T) {
	sseBody := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"weighing it up\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"final answer\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	srv := newStreamingTestServer(t, sseBody)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createResp, _ := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader(`{"title":"x"}`))
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions/"+created.ID+"/messages", bytes.NewReader([]byte(`{"content":"oi"}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var sawReasoning bool
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var evt map[string]any
		if json.Unmarshal([]byte(data), &evt) != nil {
			continue
		}
		if evt["type"] == "reasoning" {
			sawReasoning = true
			if evt["content"] != "weighing it up" {
				t.Errorf("reasoning event content = %v, want %q", evt["content"], "weighing it up")
			}
		}
	}
	if !sawReasoning {
		t.Fatal("no \"reasoning\" SSE event reached the daemon's own stream")
	}
}

func TestHandleHealthReturnsOK(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHandleCreateAndListSessions(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader(`{"title":"my session"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Title != "my session" {
		t.Errorf("created.Title = %q, want %q", created.Title, "my session")
	}

	listResp, err := http.Get(ts.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("session list = %+v, want 1 entry", list)
	}
}

func TestHandleCreateSessionWithEmptyBodyDefaultsTitle(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201 even with an empty body", resp.StatusCode)
	}
}

func TestHandleCreateSessionRejectsInvalidJSON(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for invalid JSON", resp.StatusCode)
	}
}

func TestHandleGetSessionReturns404ForUnknownID(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions/ses_does_not_exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandleGetSessionReturnsSessionAndMessages(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createResp, _ := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader(`{"title":"x"}`))
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	resp, err := http.Get(ts.URL + "/sessions/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["session"]; !ok {
		t.Errorf("body = %+v, want a \"session\" key", body)
	}
	if _, ok := body["messages"]; !ok {
		t.Errorf("body = %+v, want a \"messages\" key (null is fine for an empty history)", body)
	}
}

func TestHandleSendMessageStreamsDeltasAndDoneEvent(t *testing.T) {
	srv := newTestServer(t, []openai.ChatCompletionResponse{{
		Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "hello there"}, FinishReason: "stop"}},
	}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createResp, _ := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader(`{"title":"x"}`))
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions/"+created.ID+"/messages", bytes.NewReader([]byte(`{"content":"oi"}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	scanner := bufio.NewScanner(resp.Body)
	var sawDone bool
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			break
		}
	}
	if !sawDone {
		t.Error("expected the stream to end with a [DONE] marker")
	}
}

func TestHandleSendMessageReturns404ForUnknownSession(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions/ses_missing/messages", "application/json", strings.NewReader(`{"content":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown session ID (checked before committing to the stream)", resp.StatusCode)
	}
}

func TestHandleSendMessageRejectsEmptyContent(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createResp, _ := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader(`{"title":"x"}`))
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	resp, err := http.Post(ts.URL+"/sessions/"+created.ID+"/messages", "application/json", strings.NewReader(`{"content":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty content", resp.StatusCode)
	}
}

func TestHandleAnswerQuestionReturns404ForUnknownID(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions/ses_1/answer", "application/json", strings.NewReader(`{"question_id":"q_missing","answer":"yes"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a question_id with no pending question", resp.StatusCode)
	}
}

func TestHandleAnswerQuestionRejectsEmptyID(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions/ses_1/answer", "application/json", strings.NewReader(`{"question_id":"","answer":"yes"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an empty question_id", resp.StatusCode)
	}
}

func TestHandleAnswerApprovalReturns404ForUnknownID(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions/ses_1/approve", "application/json", strings.NewReader(`{"approval_id":"a_missing","decision":"once"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an approval_id with no pending approval", resp.StatusCode)
	}
}

func TestHandleAnswerApprovalRejectsEmptyID(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions/ses_1/approve", "application/json", strings.NewReader(`{"approval_id":"","decision":"once"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an empty approval_id", resp.StatusCode)
	}
}

func TestHandleListToolsReturnsToolsAndSkills(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	toolsList, _ := body["tools"].([]any)
	if len(toolsList) == 0 {
		t.Error("expected a non-empty tools list from the real registry")
	}
}

func TestHandleUpdateToolSettingsChangesLiveRegistry(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/tools/settings", strings.NewReader(`{"disabled":["bash"]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200", resp.StatusCode)
	}

	listResp, err := http.Get(ts.URL + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var body struct {
		Tools []tools.ToolInfo `json:"tools"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, item := range body.Tools {
		if item.Name == "bash" && !item.Disabled {
			t.Fatal("bash should be disabled in the already-running registry")
		}
	}
}

func TestHandleUpdateToolSettingsRejectsInvalidJSON(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/tools/settings", strings.NewReader("{"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleGetContextReturns404ForUnknownSession(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions/ses_missing/context")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestRecoverMiddlewareTurnsPanicIntoA500 confirms a handler panic never
// takes the whole daemon down — the documented invariant on Handler()
// itself: "a single bad request must never take the daemon down and
// orphan every session it owns."
func TestRecoverMiddlewareTurnsPanicIntoA500(t *testing.T) {
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	srv := newTestServer(t, nil) // New() defaults a nil logger to slog.Default()
	ts := httptest.NewServer(srv.recoverMiddleware(panicky))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 after a recovered panic", resp.StatusCode)
	}
}

func TestContextUsageIsReachableForRealSession(t *testing.T) {
	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createResp, _ := http.Post(ts.URL+"/sessions", "application/json", strings.NewReader(`{"title":"x"}`))
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	resp, err := http.Get(ts.URL + "/sessions/" + created.ID + "/context")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for a real session's context usage", resp.StatusCode)
	}
}

func TestProcessObserverEndpointsListIncrementalOutputAndRejectBadCursors(t *testing.T) {
	srv, registry := newTestServerWithRegistry(t, nil)
	if _, err := registry.Execute(context.Background(), "run_background", json.RawMessage(`{"command":"printf panel-ready"}`)); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	listResp, err := http.Get(ts.URL + "/processes")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var processes []tools.BackgroundProcessInfo
	if err := json.NewDecoder(listResp.Body).Decode(&processes); err != nil || len(processes) != 1 || processes[0].ID != "bg1" {
		t.Fatalf("process list = %+v, err=%v", processes, err)
	}

	var output tools.BackgroundProcessOutput
	waitUntil := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitUntil) {
		resp, getErr := http.Get(ts.URL + "/processes/bg1/output")
		if getErr != nil {
			t.Fatal(getErr)
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&output)
		resp.Body.Close()
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if strings.Contains(output.Output, "panel-ready") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(output.Output, "panel-ready") || !output.Reset {
		t.Fatalf("initial output = %+v", output)
	}

	for _, path := range []string{"/processes/bg1/output?cursor=-1", "/processes/bg1/output?cursor=nope"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, resp.StatusCode)
		}
	}
	missing, err := http.Get(ts.URL + "/processes/bg404/output")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing process status = %d, want 404", missing.StatusCode)
	}
}

func TestHandlersRejectMalformedJSON(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, path := range []string{"/sessions/x/messages", "/sessions/x/answer", "/sessions/x/approve"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{"))
		req.Host = "localhost" // httptest.NewRequest defaults to "example.com", which the daemon's Host guard rejects
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, rr.Code)
		}
	}
}

type responseWriterWithoutFlusher struct {
	header http.Header
	code   int
}

func (w *responseWriterWithoutFlusher) Header() http.Header         { return w.header }
func (w *responseWriterWithoutFlusher) Write(p []byte) (int, error) { return len(p), nil }
func (w *responseWriterWithoutFlusher) WriteHeader(code int)        { w.code = code }

func TestSendMessageRejectsWriterWithoutStreaming(t *testing.T) {
	srv := newTestServer(t, nil)
	created, err := srv.sessions.Create("x")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"content":"hi"}`))
	req.SetPathValue("id", created.ID)
	w := &responseWriterWithoutFlusher{header: make(http.Header)}
	srv.handleSendMessage(w, req)
	if w.code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.code)
	}
}

func TestStoreFailuresBecomeInternalServerErrors(t *testing.T) {
	workspace := t.TempDir()
	st, err := store.Open(filepath.Join(workspace, "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tools.NewRegistry(workspace, st, nil)
	agentSvc, err := agent.New(st, gatewayclient.New("http://127.0.0.1:1"), tr, agent.Config{Workspace: workspace, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(session.New(st), agentSvc, nil, "")
	created, err := srv.sessions.Create("before close")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/sessions", `{"title":"x"}`},
		{http.MethodGet, "/sessions", ""},
		{http.MethodGet, "/sessions/" + created.ID, ""},
		{http.MethodGet, "/sessions/" + created.ID + "/context", ""},
		{http.MethodPost, "/sessions/" + created.ID + "/messages", `{"content":"hi"}`},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Host = "localhost" // pass the daemon's Host guard — see TestHandlersRejectMalformedJSON
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("%s %s status = %d, want 500: %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

// newAuthedTestServer builds a real Server that requires the given bearer
// token — the perimeter TestGuardMiddleware exercises.
func newAuthedTestServer(t *testing.T, token string) *Server {
	t.Helper()
	workspace := t.TempDir()
	st, err := store.Open(filepath.Join(workspace, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tools.NewRegistry(workspace, st, nil)
	agentSvc, err := agent.New(st, gatewayclient.New("http://127.0.0.1:1"), tr, agent.Config{Workspace: workspace, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	return New(session.New(st), agentSvc, nil, token)
}

func TestGuardMiddlewareRejectsMissingToken(t *testing.T) {
	srv := newAuthedTestServer(t, "sekret")
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Host = "localhost"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rr.Code)
	}
}

func TestGuardMiddlewareRejectsWrongToken(t *testing.T) {
	srv := newAuthedTestServer(t, "sekret")
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rr.Code)
	}
}

func TestGuardMiddlewareAcceptsCorrectToken(t *testing.T) {
	srv := newAuthedTestServer(t, "sekret")
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer sekret")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("correct token: status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
}

func TestGuardMiddlewareRejectsNonLocalHost(t *testing.T) {
	srv := newAuthedTestServer(t, "sekret")
	// A DNS-rebinding attack arrives with an attacker hostname in Host,
	// even carrying a token it somehow guessed — the Host check must
	// reject it before that matters.
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Host = "evil.example.com"
	req.Header.Set("Authorization", "Bearer sekret")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-local Host: status = %d, want 403", rr.Code)
	}
}

func TestGuardMiddlewareHealthExemptFromAuth(t *testing.T) {
	srv := newAuthedTestServer(t, "sekret")
	// /health carries no token and any Host — it's a readiness probe used
	// before a client knows the token, and exposes nothing.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Host = "example.com"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200 without auth", rr.Code)
	}
}
