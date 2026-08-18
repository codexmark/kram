package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestClient_InitializeHandshake(t *testing.T) {
	client, fs := newFakeServerPair(t, "go")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.initialize(ctx, "/tmp/workspace"); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	methods := fs.waitForMethodCount(t, 2, 2*time.Second)
	if len(methods) != 2 {
		t.Fatalf("expected 2 messages (initialize request + initialized notification), got %v", methods)
	}
	if methods[0] != "initialize" {
		t.Fatalf("first message should be initialize, got %q", methods[0])
	}
	if methods[1] != "initialized" {
		t.Fatalf("second message should be initialized notification, got %q", methods[1])
	}
}

func TestClient_InitializeFailure(t *testing.T) {
	client, fs := newFakeServerPair(t, "go")
	fs.handlers["initialize"] = func(fs *fakeServer, env envelope) {
		fs.write(mustMarshal(t, rpcResponse{
			JSONRPC: jsonRPCVersion, ID: env.ID,
			Error: &rpcError{Code: -32603, Message: "boom"},
		}))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.initialize(ctx, "/tmp/workspace")
	if err == nil {
		t.Fatal("expected an error when the server rejects initialize")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestClient_RequestIDCorrelation fires several concurrent requests whose
// responses the fake server deliberately answers out of order, and checks
// each caller gets back exactly the result meant for its own request —
// proving IDs are generated distinctly and responses are routed to the
// right waiter rather than handed out first-come-first-served.
func TestClient_RequestIDCorrelation(t *testing.T) {
	client, fs := newFakeServerPair(t, "go")

	fs.handlers["test/echo"] = func(fs *fakeServer, env envelope) {
		var params struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal(env.Params, &params)
		go func() {
			// Answer in reverse-ish order to stress correlation rather
			// than relying on accidental in-order delivery.
			time.Sleep(time.Duration(5-params.N) * time.Millisecond)
			fs.respondResult(env.ID, map[string]any{"n": params.N})
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			var out struct {
				N int `json:"n"`
			}
			if err := client.call(ctx, "test/echo", map[string]any{"n": n}, &out); err != nil {
				errs[n] = err
				return
			}
			if out.N != n {
				errs[n] = fmt.Errorf("response correlated to wrong request: want n=%d got n=%d", n, out.N)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
}

func TestClient_DiagnosticsPublished(t *testing.T) {
	client, fs := newFakeServerPair(t, "go")
	fs.handlers["textDocument/didOpen"] = func(fs *fakeServer, env envelope) {
		var params didOpenParams
		_ = json.Unmarshal(env.Params, &params)
		fs.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{
			URI: params.TextDocument.URI,
			Diagnostics: []Diagnostic{
				{Range: Range{Start: Position{Line: 4, Character: 1}}, Severity: SeverityError, Message: "undefined: foo"},
			},
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.initialize(ctx, "/tmp/workspace"); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	uri := "file:///tmp/workspace/main.go"
	if err := client.ensureOpen(ctx, uri, "go", "package main\n"); err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}

	diags := client.waitDiagnostics(ctx, uri, 2*time.Second)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d: %+v", len(diags), diags)
	}
	if diags[0].Message != "undefined: foo" || diags[0].Severity != SeverityError {
		t.Fatalf("unexpected diagnostic: %+v", diags[0])
	}
	if diags[0].Range.Start.Line != 4 || diags[0].Range.Start.Character != 1 {
		t.Fatalf("unexpected range: %+v", diags[0].Range)
	}
}

func TestClient_ReopenSendsDidCloseFirst(t *testing.T) {
	client, fs := newFakeServerPair(t, "go")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.initialize(ctx, "/tmp/workspace"); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	uri := "file:///tmp/workspace/main.go"
	if err := client.ensureOpen(ctx, uri, "go", "v1"); err != nil {
		t.Fatalf("first ensureOpen: %v", err)
	}
	if err := client.ensureOpen(ctx, uri, "go", "v2"); err != nil {
		t.Fatalf("second ensureOpen: %v", err)
	}

	// 4 messages total: initialize, initialized, didOpen, didClose, didOpen.
	// Wait for all 5 to land before counting.
	methods := fs.waitForMethodCount(t, 5, 2*time.Second)

	var didOpenCount, didCloseCount int
	for _, m := range methods {
		switch m {
		case "textDocument/didOpen":
			didOpenCount++
		case "textDocument/didClose":
			didCloseCount++
		}
	}
	if didOpenCount != 2 {
		t.Fatalf("expected 2 didOpen, got %d (methods: %v)", didOpenCount, methods)
	}
	if didCloseCount != 1 {
		t.Fatalf("expected 1 didClose (before the second open), got %d (methods: %v)", didCloseCount, methods)
	}
}

func TestClient_Definition(t *testing.T) {
	client, fs := newFakeServerPair(t, "go")
	fs.handlers["textDocument/definition"] = func(fs *fakeServer, env envelope) {
		fs.respondResult(env.ID, Location{
			URI:   "file:///tmp/workspace/other.go",
			Range: Range{Start: Position{Line: 9, Character: 5}, End: Position{Line: 9, Character: 8}},
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.initialize(ctx, "/tmp/workspace"); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	locs, err := client.definition(ctx, "file:///tmp/workspace/main.go", Position{Line: 2, Character: 3})
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location, got %d", len(locs))
	}
	if locs[0].URI != "file:///tmp/workspace/other.go" || locs[0].Range.Start.Line != 9 {
		t.Fatalf("unexpected location: %+v", locs[0])
	}
}

func TestClient_References(t *testing.T) {
	client, fs := newFakeServerPair(t, "go")
	fs.handlers["textDocument/references"] = func(fs *fakeServer, env envelope) {
		fs.respondResult(env.ID, []Location{
			{URI: "file:///tmp/workspace/a.go", Range: Range{Start: Position{Line: 1, Character: 0}}},
			{URI: "file:///tmp/workspace/b.go", Range: Range{Start: Position{Line: 20, Character: 4}}},
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.initialize(ctx, "/tmp/workspace"); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	locs, err := client.references(ctx, "file:///tmp/workspace/a.go", Position{Line: 1, Character: 0}, true)
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(locs))
	}
	if locs[0].URI != "file:///tmp/workspace/a.go" || locs[1].URI != "file:///tmp/workspace/b.go" {
		t.Fatalf("unexpected locations: %+v", locs)
	}
}

// TestClient_ServerInitiatedRequestGetsAnswered proves the client answers
// a server -> client request (workspace/configuration, the one gopls
// actually sends during startup) rather than ignoring it — some server
// implementations block waiting for that reply, so silently dropping it
// would hang the real server even though this client never asked it a
// question.
func TestClient_ServerInitiatedRequestGetsAnswered(t *testing.T) {
	client, fs := newFakeServerPair(t, "go")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.initialize(ctx, "/tmp/workspace"); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	fs.write(mustMarshal(t, struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  any             `json:"params"`
	}{JSONRPC: jsonRPCVersion, ID: json.RawMessage(`99`), Method: "workspace/configuration", Params: map[string]any{
		"items": []any{map[string]any{"section": "go"}},
	}}))

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("client never answered the server-initiated workspace/configuration request")
		default:
		}

		fs.mu.Lock()
		var result json.RawMessage
		found := false
		for _, env := range fs.received {
			if string(env.ID) == "99" && env.Method == "" {
				found, result = true, env.Result
				break
			}
		}
		fs.mu.Unlock()

		if found {
			var arr []any
			if err := json.Unmarshal(result, &arr); err != nil {
				t.Fatalf("expected an array result, got %s: %v", result, err)
			}
			if len(arr) != 1 {
				t.Fatalf("expected a 1-element result matching the 1 requested item, got %d", len(arr))
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
