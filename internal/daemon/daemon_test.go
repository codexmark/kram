package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestRunStartsAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	err := Run(ctx, Config{Host: "127.0.0.1", Port: freePort(t), DBPath: filepath.Join(t.TempDir(), "daemon.db"), GatewayURL: "http://127.0.0.1:1", Workspace: t.TempDir(), MaxTurns: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunReportsStoreOpenFailure(t *testing.T) {
	err := Run(context.Background(), Config{DBPath: t.TempDir(), Workspace: t.TempDir()}, nil)
	if err == nil {
		t.Fatal("Run with directory DB path succeeded")
	}
}

func waitForDaemon(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon never became healthy")
}

// TestRunThreadsPreferStreamingToGateway confirms the real daemon
// construction site (this file's own Run) actually passes
// Config.PreferStreaming through to agent.Config.PreferStreaming, in
// both directions — the caller (cmd/kram's -stream flag, cmd/daemon's
// own) decides, not a value hardcoded inside Run. The opt-out direction
// exists specifically because a deployment can get stuck otherwise: a
// slow local model whose inference server sends nothing at all during
// prompt prefill trips router.BoundedPeek's fixed idle timeout on the
// streaming path well before the first token streams, failing every
// turn outright — see Config.PreferStreaming's own doc comment.
func TestRunThreadsPreferStreamingToGateway(t *testing.T) {
	for _, preferStreaming := range []bool{true, false} {
		t.Run(fmt.Sprintf("PreferStreaming=%v", preferStreaming), func(t *testing.T) {
			var mu sync.Mutex
			var sawStream bool
			gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req openai.ChatCompletionRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				mu.Lock()
				sawStream = req.Stream
				mu.Unlock()
				if req.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
					Choices: []openai.ChatCompletionChoice{{Message: openai.ChatMessage{Role: "assistant", Content: "hi"}}},
				})
			}))
			defer gwSrv.Close()

			port := freePort(t)
			const token = "test-daemon-token"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, Config{
					Host: "127.0.0.1", Port: port, DBPath: filepath.Join(t.TempDir(), "daemon.db"),
					GatewayURL: gwSrv.URL, Workspace: t.TempDir(), MaxTurns: 1,
					PreferStreaming: preferStreaming,
					AuthToken:       token,
				}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			}()

			baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
			waitForDaemon(t, baseURL)

			// The daemon now requires the bearer token on every route
			// except /health — send it, as a real client would.
			post := func(path, body string) *http.Response {
				req, _ := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				return resp
			}

			createResp := post("/sessions", `{"title":"x"}`)
			var created struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(createResp.Body).Decode(&created)
			createResp.Body.Close()

			msgResp := post("/sessions/"+created.ID+"/messages", `{"content":"oi"}`)
			_, _ = io.Copy(io.Discard, msgResp.Body)
			msgResp.Body.Close()

			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Run: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if sawStream != preferStreaming {
				t.Errorf("gateway request had Stream=%v, want %v to match Config.PreferStreaming", sawStream, preferStreaming)
			}
		})
	}
}
