package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
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

// gatedTestServer wires a real daemon Server whose fake gateway blocks
// each model call until release is signaled — the controllable "long
// turn" the detachable-turn contract tests need.
func gatedTestServer(t *testing.T) (*Server, *store.Store, chan struct{}) {
	t.Helper()
	workspace := t.TempDir()
	st, err := store.Open(filepath.Join(workspace, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatMessage{Role: "assistant", Content: "finished after detach"}, FinishReason: "stop",
			}},
		})
	}))
	t.Cleanup(gwSrv.Close)

	tr := tools.NewRegistry(workspace, st, nil)
	agentSvc, err := agent.New(st, gatewayclient.New(gwSrv.URL), tr, agent.Config{Workspace: workspace, MaxTurns: 3, MaxGatewayRounds: 1})
	if err != nil {
		t.Fatal(err)
	}
	return New(session.New(st), agentSvc, nil, ""), st, release
}

func createTurnSession(t *testing.T, api *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(api.URL+"/sessions", "application/json", strings.NewReader(`{"title":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

// TestTurnSurvivesClientDetach (#112): closing the SSE stream must NOT
// cancel the run — the answer still lands in the session afterwards.
func TestTurnSurvivesClientDetach(t *testing.T) {
	s, st, release := gatedTestServer(t)
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	sessID := createTurnSession(t, api)

	// Start the turn and immediately drop the connection.
	ctx, cancelReq := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+"/sessions/"+sessID+"/messages", strings.NewReader(`{"content":"hi"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cancelReq() // client gone

	close(release) // model finally answers, with nobody watching

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := st.ListMessages(sessID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.Role == "assistant" && m.Content == "finished after detach" {
				return // the run completed without any client attached
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the detached turn never finished — closing the stream still cancels the run")
}

// TestReattachReplaysAndCompletes: a second client attaching mid-turn
// gets the replayed frames and the terminal done event.
func TestReattachReplaysAndCompletes(t *testing.T) {
	s, _, release := gatedTestServer(t)
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	sessID := createTurnSession(t, api)

	ctx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+"/sessions/"+sessID+"/messages", strings.NewReader(`{"content":"hi"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cancelReq()

	// Reattach while the model is still "thinking" (gateway blocked).
	attach, err := http.Get(api.URL + "/sessions/" + sessID + "/turn")
	if err != nil {
		t.Fatal(err)
	}
	defer attach.Body.Close()
	go func() { time.Sleep(50 * time.Millisecond); close(release) }()

	var sawDone bool
	scanner := bufio.NewScanner(attach.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, `"type":"done"`) && strings.Contains(line, "finished after detach") {
			sawDone = true
		}
		if strings.Contains(line, "[DONE]") {
			break
		}
	}
	if !sawDone {
		t.Fatal("reattached client never received the terminal done event")
	}

	// After completion, a fresh attach still works within the retention
	// window and replays through to [DONE].
	attach2, err := http.Get(api.URL + "/sessions/" + sessID + "/turn")
	if err != nil {
		t.Fatal(err)
	}
	defer attach2.Body.Close()
	body, _ := io.ReadAll(attach2.Body)
	if !strings.Contains(string(body), `"type":"done"`) || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("post-completion reattach lost the replay: %s", string(body))
	}
}

// TestInterruptCancelsDetachedTurn: the explicit interrupt endpoint is
// what stops a run now.
func TestInterruptCancelsDetachedTurn(t *testing.T) {
	s, st, release := gatedTestServer(t)
	defer close(release) // never answers on its own
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	sessID := createTurnSession(t, api)

	ctx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+"/sessions/"+sessID+"/messages", strings.NewReader(`{"content":"hi"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cancelReq()

	ir, err := http.Post(api.URL+"/sessions/"+sessID+"/interrupt", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	ir.Body.Close()
	if ir.StatusCode != http.StatusOK {
		t.Fatalf("interrupt status = %d", ir.StatusCode)
	}

	// The run ends (with an error, since the model call was canceled) and
	// no assistant answer is ever persisted.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tt := s.turns.get(sessID); tt == nil || tt.isDone() {
			msgs, _ := st.ListMessages(sessID)
			for _, m := range msgs {
				if m.Role == "assistant" && strings.Contains(m.Content, "finished") {
					t.Fatal("interrupted turn still produced an answer")
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("interrupt did not end the turn")
}
