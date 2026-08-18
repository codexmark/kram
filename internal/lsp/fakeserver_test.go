package lsp

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeServer is a minimal, scriptable in-process LSP server speaking the
// real Content-Length framing and JSON-RPC envelope over a net.Pipe end —
// this is what lets every test in this package exercise real protocol
// behavior (handshake, request correlation, diagnostics push, definition/
// references responses) without gopls, typescript-language-server, or
// pyright installed anywhere near the test machine.
type fakeServer struct {
	t    *testing.T
	conn net.Conn

	writeMu sync.Mutex

	mu       sync.Mutex
	received []envelope
	handlers map[string]func(*fakeServer, envelope)
}

// newFakeServerPair returns a connected (*Client, *fakeServer) pair: the
// Client end talks real LSP framing over one side of a net.Pipe, the
// fakeServer end drives the other side. The initialize handler responds
// successfully by default; override handlers["initialize"] before calling
// Client methods to test a different handshake outcome.
func newFakeServerPair(t *testing.T, lang string) (*Client, *fakeServer) {
	t.Helper()
	clientConn, serverConn := net.Pipe()

	fs := &fakeServer{t: t, conn: serverConn, handlers: make(map[string]func(*fakeServer, envelope))}
	fs.handlers["initialize"] = func(fs *fakeServer, env envelope) {
		fs.respondResult(env.ID, initializeResult{Capabilities: json.RawMessage(`{}`)})
	}
	go fs.loop()

	client := newClient(lang, clientConn)
	t.Cleanup(func() {
		client.Close()
		_ = serverConn.Close()
	})
	return client, fs
}

func (fs *fakeServer) loop() {
	r := bufio.NewReader(fs.conn)
	for {
		raw, err := readFrame(r)
		if err != nil {
			return
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		fs.mu.Lock()
		fs.received = append(fs.received, env)
		handler := fs.handlers[env.Method]
		fs.mu.Unlock()

		if handler != nil {
			handler(fs, env)
			continue
		}
		// Unhandled but expects a reply: answer with an empty result so
		// the client never hangs on something the test didn't script.
		if len(env.ID) > 0 {
			fs.respondResult(env.ID, map[string]any{})
		}
	}
}

func (fs *fakeServer) respondResult(id json.RawMessage, result any) {
	payload, err := json.Marshal(rpcResponse{JSONRPC: jsonRPCVersion, ID: id, Result: result})
	if err != nil {
		fs.t.Fatalf("fakeServer: marshal response: %v", err)
	}
	fs.write(payload)
}

func (fs *fakeServer) notify(method string, params any) {
	payload, err := json.Marshal(rpcNotification{JSONRPC: jsonRPCVersion, Method: method, Params: params})
	if err != nil {
		fs.t.Fatalf("fakeServer: marshal notification: %v", err)
	}
	fs.write(payload)
}

func (fs *fakeServer) write(payload []byte) {
	fs.writeMu.Lock()
	defer fs.writeMu.Unlock()
	if err := writeFrame(fs.conn, payload); err != nil {
		// The client side may already have closed as part of test
		// cleanup; that's not a test failure.
		return
	}
}

// callsTo returns how many times method was received, for lazy-lifecycle
// and correlation assertions.
func (fs *fakeServer) callsTo(method string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := 0
	for _, env := range fs.received {
		if env.Method == method {
			n++
		}
	}
	return n
}

// waitForMethodCount polls until fs has recorded at least n received
// messages or timeout elapses, then returns whatever it has. A
// notify()/call() returning on the client side only guarantees the bytes
// crossed the pipe, not that the fake server's read-loop goroutine has
// finished parsing and recording them yet, so tests that assert on
// fs.received right after a client call needs this rather than checking
// immediately.
func (fs *fakeServer) waitForMethodCount(t *testing.T, n int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		methods := fs.methodsReceived()
		if len(methods) >= n || time.Now().After(deadline) {
			return methods
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (fs *fakeServer) methodsReceived() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]string, len(fs.received))
	for i, env := range fs.received {
		out[i] = env.Method
	}
	return out
}
