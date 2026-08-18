package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeTransport implements transport entirely in-process — the point is
// to test Client's request/response correlation, handshake sequencing,
// and tool-call flattening without a real subprocess or network call
// (which is what devtools/mock-mcp is for, manually — that directory is
// gitignored and unavailable to `go test` in a fresh clone).
type fakeTransport struct {
	in      chan message
	handle  func(method string, id *int64, params json.RawMessage) (result any, isErr bool)
	sentRaw []string
	closed  bool
}

func newFakeTransport(handle func(method string, id *int64, params json.RawMessage) (any, bool)) *fakeTransport {
	return &fakeTransport{in: make(chan message, 16), handle: handle}
}

func (f *fakeTransport) Send(ctx context.Context, payload []byte) error {
	f.sentRaw = append(f.sentRaw, string(payload))

	var envelope struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return err
	}
	if envelope.ID == nil {
		return nil // a notification — no reply expected
	}
	if f.handle == nil {
		return nil // sent, but nothing ever replies — used to test the no-response path
	}

	result, isErr := f.handle(envelope.Method, envelope.ID, envelope.Params)
	resultData, _ := json.Marshal(result)
	msg := message{ID: envelope.ID}
	if isErr {
		msg.Error = &rpcError{Code: -32000, Message: "fake error"}
	} else {
		msg.Result = resultData
	}
	f.in <- msg
	return nil
}

func (f *fakeTransport) Recv() <-chan message { return f.in }
func (f *fakeTransport) Close() error         { f.closed = true; close(f.in); return nil }

// newTestClient builds a Client wired to a fakeTransport and runs
// dispatch, bypassing Connect (which would try to actually start a
// process or dial a URL).
func newTestClient(handle func(method string, id *int64, params json.RawMessage) (any, bool)) (*Client, *fakeTransport) {
	tr := newFakeTransport(handle)
	c := &Client{name: "test", transport: tr, pending: make(map[int64]chan message)}
	go c.dispatch()
	return c, tr
}

func TestClientHandshakeAndListTools(t *testing.T) {
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		switch method {
		case "initialize":
			return initializeResult{ProtocolVersion: "2025-11-25"}, false
		case "tools/list":
			return listToolsResult{Tools: []Tool{
				{Name: "echo", Description: "echoes input", InputSchema: json.RawMessage(`{"type":"object"}`)},
			}}, false
		}
		t.Fatalf("unexpected method %q", method)
		return nil, false
	})
	defer c.Close()

	ctx := context.Background()
	if err := c.handshake(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.loadTools(ctx); err != nil {
		t.Fatal(err)
	}

	if len(c.Tools()) != 1 || c.Tools()[0].Name != "echo" {
		t.Fatalf("expected one tool named echo, got %+v", c.Tools())
	}
}

func TestClientListToolsPagination(t *testing.T) {
	calls := 0
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		if method != "tools/list" {
			t.Fatalf("unexpected method %q", method)
		}
		calls++
		if calls == 1 {
			return listToolsResult{
				Tools:      []Tool{{Name: "first"}},
				NextCursor: "page2",
			}, false
		}
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Cursor != "page2" {
			t.Errorf("expected the second call to echo back cursor=page2, got %q", p.Cursor)
		}
		return listToolsResult{Tools: []Tool{{Name: "second"}}}, false
	})
	defer c.Close()

	if err := c.loadTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.Tools()) != 2 {
		t.Fatalf("expected tools from both pages, got %+v", c.Tools())
	}
	if calls != 2 {
		t.Errorf("expected exactly 2 tools/list calls, got %d", calls)
	}
}

func TestClientCallToolFlattensTextContent(t *testing.T) {
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		if method != "tools/call" {
			t.Fatalf("unexpected method %q", method)
		}
		return callToolResult{Content: []contentBlock{{Type: "text", Text: "the answer"}}}, false
	})
	defer c.Close()

	out, err := c.CallTool(context.Background(), "whatever", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "the answer" {
		t.Errorf("got %q, want %q", out, "the answer")
	}
}

func TestClientCallToolIsErrorPrefixesResult(t *testing.T) {
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		return callToolResult{Content: []contentBlock{{Type: "text", Text: "bad input"}}, IsError: true}, false
	})
	defer c.Close()

	out, err := c.CallTool(context.Background(), "x", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err) // tool-level errors come back as text, not a Go error — matches Kram's built-in tool contract
	}
	if !strings.HasPrefix(out, "error:") {
		t.Errorf("expected the result to be prefixed with 'error:', got %q", out)
	}
}

func TestClientCallToolProtocolErrorReturnsGoError(t *testing.T) {
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		return nil, true // simulate a JSON-RPC level error
	})
	defer c.Close()

	if _, err := c.CallTool(context.Background(), "x", json.RawMessage(`{}`)); err == nil {
		t.Error("a JSON-RPC protocol error should surface as a Go error from call()")
	}
}

// silentTransport accepts a Send but never produces a reply — the "hung
// server" case call() has to survive without blocking forever.
type silentTransport struct{ in chan message }

func (s *silentTransport) Send(context.Context, []byte) error { return nil }
func (s *silentTransport) Recv() <-chan message               { return s.in }
func (s *silentTransport) Close() error                       { return nil }

func TestClientCallReturnsPromptlyOnCanceledContext(t *testing.T) {
	tr := &silentTransport{in: make(chan message)}
	c := &Client{name: "silent", transport: tr, pending: make(map[int64]chan message)}
	go c.dispatch()

	// Nothing ever arrives on tr.in, so the only way call() can return is
	// via ctx.Done() or the real 60s callTimeout — canceling up front
	// makes this deterministic instead of racing a live reply.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CallTool(ctx, "x", json.RawMessage(`{}`)); err == nil {
		t.Error("call() should return promptly when ctx is already canceled, not hang")
	}
}

func TestClientListResourcesPagination(t *testing.T) {
	calls := 0
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		if method != "resources/list" {
			t.Fatalf("unexpected method %q", method)
		}
		calls++
		if calls == 1 {
			return listResourcesResult{Resources: []Resource{{URI: "file:///a"}}, NextCursor: "p2"}, false
		}
		return listResourcesResult{Resources: []Resource{{URI: "file:///b"}}}, false
	})
	defer c.Close()

	resources, err := c.ListResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 {
		t.Fatalf("expected resources from both pages, got %+v", resources)
	}
}

func TestClientReadResourceFlattensText(t *testing.T) {
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		if method != "resources/read" {
			t.Fatalf("unexpected method %q", method)
		}
		var p readResourceParams
		_ = json.Unmarshal(params, &p)
		if p.URI != "file:///x" {
			t.Errorf("uri = %q, want file:///x", p.URI)
		}
		return readResourceResult{Contents: []resourceContent{{URI: p.URI, Text: "file contents"}}}, false
	})
	defer c.Close()

	out, err := c.ReadResource(context.Background(), "file:///x")
	if err != nil {
		t.Fatal(err)
	}
	if out != "file contents" {
		t.Errorf("got %q, want %q", out, "file contents")
	}
}

func TestClientReadResourceBinaryPlaceholder(t *testing.T) {
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		return readResourceResult{Contents: []resourceContent{{MimeType: "image/png", Blob: "base64=="}}}, false
	})
	defer c.Close()

	out, err := c.ReadResource(context.Background(), "file:///x.png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "image/png") || !strings.Contains(out, "not renderable") {
		t.Errorf("expected a binary placeholder mentioning the mime type, got %q", out)
	}
}

func TestClientListPromptsAndGetPrompt(t *testing.T) {
	c, _ := newTestClient(func(method string, id *int64, params json.RawMessage) (any, bool) {
		switch method {
		case "prompts/list":
			return listPromptsResult{Prompts: []Prompt{{Name: "greeting", Description: "says hi"}}}, false
		case "prompts/get":
			var p getPromptParams
			_ = json.Unmarshal(params, &p)
			if p.Arguments["name"] != "Mark" {
				t.Errorf("expected arguments to be forwarded, got %+v", p.Arguments)
			}
			return getPromptResult{
				Description: "a greeting",
				Messages: []promptMessage{{Role: "user", Content: struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{Type: "text", Text: "Hello, Mark!"}}},
			}, false
		}
		t.Fatalf("unexpected method %q", method)
		return nil, false
	})
	defer c.Close()

	prompts, err := c.ListPrompts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0].Name != "greeting" {
		t.Fatalf("expected one prompt named greeting, got %+v", prompts)
	}

	out, err := c.GetPrompt(context.Background(), "greeting", map[string]string{"name": "Mark"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Hello, Mark!") {
		t.Errorf("expected the resolved prompt message, got %q", out)
	}
}

func TestClientDisconnectUnblocksPendingCalls(t *testing.T) {
	tr := &fakeTransport{in: make(chan message, 1)}
	c := &Client{name: "x", transport: tr, pending: make(map[int64]chan message)}
	go c.dispatch()

	done := make(chan error, 1)
	go func() {
		done <- c.call(context.Background(), "tools/list", nil, nil)
	}()

	time.Sleep(20 * time.Millisecond) // let call() register its pending entry
	close(tr.in)                      // simulate the server process dying

	select {
	case err := <-done:
		if err == nil {
			t.Error("a closed transport should surface as an error, not hang forever")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call() did not return after the transport closed — dispatch's cleanup path is broken")
	}
}
