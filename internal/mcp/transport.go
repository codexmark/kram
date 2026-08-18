package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// transport moves JSON-RPC messages to and from one server. Send/Recv
// rather than a single Call(req) (resp, error): both transports interleave
// server-initiated notifications with responses on the same stream, so
// correlating a response to its request is the client's job (see
// Client.call), not the transport's.
type transport interface {
	Send(ctx context.Context, payload []byte) error
	Recv() <-chan message
	Close() error
}

// --- stdio ---

// stdioTransport speaks newline-delimited JSON over a child process's
// stdin/stdout — one JSON-RPC message per line, no framing headers (this
// is the actual MCP stdio wire format; it is not LSP-style). The child's
// stderr is free-form logging by spec, so it's forwarded to Kram's own
// stderr rather than parsed or treated as failure.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    chan message
	closed sync.Once
}

func newStdioTransport(ctx context.Context, command string, args []string, env map[string]string) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", command, err)
	}

	t := &stdioTransport{cmd: cmd, stdin: stdin, out: make(chan message, 16)}
	go t.readLoop(stdout)
	return t, nil
}

func (t *stdioTransport) readLoop(stdout io.Reader) {
	defer close(t.out)
	scanner := bufio.NewScanner(stdout)
	// MCP servers routinely return large tool results; the default 64KB
	// token limit would silently truncate them into parse failures.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // not a JSON-RPC message (stray output) — skip, don't kill the session
		}
		t.out <- msg
	}
}

func (t *stdioTransport) Send(ctx context.Context, payload []byte) error {
	_, err := t.stdin.Write(append(payload, '\n'))
	return err
}

func (t *stdioTransport) Recv() <-chan message { return t.out }

func (t *stdioTransport) Close() error {
	t.closed.Do(func() {
		// Spec shutdown order: close stdin first and let the server exit
		// on its own; only kill if it doesn't.
		_ = t.stdin.Close()
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		_ = t.cmd.Wait()
	})
	return nil
}

// --- streamable HTTP ---

// httpTransport speaks the Streamable HTTP transport: every client
// message is a POST, and the server answers either with a single JSON
// object or with an SSE stream scoped to that one request. Session state
// (the Mcp-Session-Id header a server may mint during initialize) is
// echoed back on later requests, which is what the pre-2026 revisions
// require.
type httpTransport struct {
	url       string
	headers   map[string]string
	http      *http.Client
	out       chan message
	sessionID string
	mu        sync.Mutex
	closeOnce sync.Once
}

func newHTTPTransport(url string, headers map[string]string) *httpTransport {
	return &httpTransport{
		url:     url,
		headers: headers,
		http:    &http.Client{},
		out:     make(chan message, 16),
	}
}

func (t *httpTransport) Send(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	t.mu.Unlock()

	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	// 202 with no body is the correct answer to a notification.
	if resp.StatusCode == http.StatusAccepted {
		resp.Body.Close()
		return nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return fmt.Errorf("mcp http %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	go t.readResponse(resp)
	return nil
}

// readResponse handles both answer shapes: a bare JSON object, or an SSE
// stream whose `data:` lines each carry one JSON-RPC message.
func (t *httpTransport) readResponse(resp *http.Response) {
	defer resp.Body.Close()

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var msg message
		if err := json.NewDecoder(resp.Body).Decode(&msg); err == nil {
			t.out <- msg
		}
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var msg message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			continue
		}
		t.out <- msg
	}
}

func (t *httpTransport) Recv() <-chan message { return t.out }

func (t *httpTransport) Close() error {
	t.closeOnce.Do(func() { close(t.out) })
	return nil
}
