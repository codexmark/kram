package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// callTimeout bounds one request/response round-trip. A language server is
// a third-party process Kram doesn't control; without this, one hung or
// misbehaving server would park a tool call (and the agent turn using it)
// indefinitely.
const callTimeout = 30 * time.Second

// stream is what a Client speaks Content-Length-framed JSON-RPC over. In
// production it's a child process's stdin+stdout; tests substitute a
// net.Pipe() end wired to a fake in-process server, satisfying the "no
// gopls/tsserver/pyright required for unit tests" requirement.
type stream interface {
	io.Reader
	io.Writer
	io.Closer
}

// procStream adapts a child process's separate stdin/stdout pipes to the
// single stream interface, and owns killing the process on Close.
type procStream struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (s *procStream) Read(p []byte) (int, error)  { return s.stdout.Read(p) }
func (s *procStream) Write(p []byte) (int, error) { return s.stdin.Write(p) }
func (s *procStream) Close() error {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
	return nil
}

// startProcess launches command as a language server subprocess and wraps
// its stdio as a stream. The caller is responsible for handling a
// non-nil error as "server unavailable", never fatal.
func startProcess(command string, args []string, workspace string) (stream, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = workspace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Server-side logging is free-form by convention (same as MCP stdio
	// servers) — forwarded to Kram's own stderr rather than parsed.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", command, err)
	}
	return &procStream{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// Client is one live connection to one language server process. Every
// exported method is safe to call from multiple goroutines.
type Client struct {
	lang   string
	stream stream

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan envelope
	dead    error // set once the read loop exits; every call fails fast after that

	docMu   sync.Mutex
	docOpen map[string]bool
	docVer  map[string]int

	diagMu      sync.Mutex
	diagnostics map[string][]Diagnostic
	diagWaiters map[string][]chan struct{}

	closeOnce sync.Once
}

func newClient(lang string, s stream) *Client {
	c := &Client{
		lang:        lang,
		stream:      s,
		pending:     make(map[int64]chan envelope),
		docOpen:     make(map[string]bool),
		docVer:      make(map[string]int),
		diagnostics: make(map[string][]Diagnostic),
		diagWaiters: make(map[string][]chan struct{}),
	}
	go c.readLoop()
	return c
}

// initialize runs the initialize -> initialized handshake against
// rootPath (the workspace directory, as a filesystem path — converted to
// a file:// URI internally). rootURIOverride lets callers pass a
// pre-built file:// URI directly to avoid double conversion.
func (c *Client) initialize(ctx context.Context, workspaceDir string) error {
	rootURI := pathToURI(workspaceDir)
	var res initializeResult
	err := c.call(ctx, "initialize", initializeParams{
		ProcessID: os.Getpid(),
		RootURI:   rootURI,
		RootPath:  workspaceDir,
		Capabilities: map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": false},
				"publishDiagnostics": map[string]any{},
				"definition":         map[string]any{},
				"references":         map[string]any{},
			},
		},
		ClientInfo: clientInfo{Name: "kram", Version: "0.1"},
	}, &res)
	if err != nil {
		return err
	}
	return c.notify(ctx, "initialized", map[string]any{})
}

// readLoop is the single reader for this connection: it parses each
// framed message and routes it to whoever's waiting (a pending call, or
// the notification/server-request handlers). Exits when the stream
// closes or a frame fails to parse in a way that desyncs the stream.
func (c *Client) readLoop() {
	r := bufio.NewReader(c.stream)
	for {
		raw, err := readFrame(r)
		if err != nil {
			c.shutdown(err)
			return
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // malformed body — skip, framing is still in sync
		}
		c.route(env)
	}
}

func (c *Client) route(env envelope) {
	switch {
	case len(env.ID) == 0:
		c.handleNotification(env)
	case env.Method != "":
		c.handleServerRequest(env)
	default:
		var id int64
		if err := json.Unmarshal(env.ID, &id); err != nil {
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ok {
			ch <- env
		}
	}
}

// handleNotification handles server -> client notifications. Only
// textDocument/publishDiagnostics is acted on; everything else
// (window/logMessage, $/progress, telemetry/event, ...) is spec-legal
// noise this v1 doesn't need and safely ignores.
func (c *Client) handleNotification(env envelope) {
	if env.Method != "textDocument/publishDiagnostics" {
		return
	}
	var params publishDiagnosticsParams
	if err := json.Unmarshal(env.Params, &params); err != nil {
		return
	}
	c.diagMu.Lock()
	c.diagnostics[params.URI] = params.Diagnostics
	waiters := c.diagWaiters[params.URI]
	delete(c.diagWaiters, params.URI)
	c.diagMu.Unlock()
	for _, w := range waiters {
		close(w)
	}
}

// handleServerRequest answers a request the server sent us. Real servers
// (gopls in particular) routinely ask workspace/configuration or
// client/registerCapability during/after initialize and block waiting for
// a reply; never answering would hang the server's own initialization on
// some implementations. Anything unrecognized gets a spec-correct "method
// not found" so the server can still proceed rather than wait forever.
func (c *Client) handleServerRequest(env envelope) {
	switch env.Method {
	case "workspace/configuration":
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(env.Params, &params)
		result := make([]any, len(params.Items))
		c.respond(env.ID, result, nil)
	case "client/registerCapability", "client/unregisterCapability", "window/workDoneProgress/create":
		c.respond(env.ID, nil, nil)
	default:
		c.respond(env.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + env.Method})
	}
}

// respond answers a server-initiated request. Built with two distinct
// anonymous struct shapes (rather than one struct relying on
// `omitempty`) so the wire message always carries exactly one of
// result/error, per the JSON-RPC 2.0 spec — in particular so an
// intentional null result (client/registerCapability's reply) is sent as
// an explicit `"result":null`, not silently dropped the way `omitempty`
// would drop a nil `any`.
func (c *Client) respond(id json.RawMessage, result any, rpcErr *rpcError) {
	var payload []byte
	var err error
	if rpcErr != nil {
		payload, err = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *rpcError       `json:"error"`
		}{JSONRPC: jsonRPCVersion, ID: id, Error: rpcErr})
	} else {
		payload, err = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  any             `json:"result"`
		}{JSONRPC: jsonRPCVersion, ID: id, Result: result})
	}
	if err != nil {
		return
	}
	c.writeMu.Lock()
	_ = writeFrame(c.stream, payload)
	c.writeMu.Unlock()
}

// shutdown runs once, when the read loop exits: it records why, and
// unblocks every call still waiting on a response rather than leaving
// them parked until their own timeout.
func (c *Client) shutdown(err error) {
	c.mu.Lock()
	if c.dead == nil {
		if err == nil || err == io.EOF {
			err = fmt.Errorf("lsp: %s server disconnected", c.lang)
		}
		c.dead = err
	}
	pending := c.pending
	c.pending = make(map[int64]chan envelope)
	c.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.dead != nil {
		d := c.dead
		c.mu.Unlock()
		return d
	}
	c.nextID++
	id := c.nextID
	ch := make(chan envelope, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(rpcRequest{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	err = writeFrame(c.stream, payload)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("sending %s: %w", method, err)
	}

	select {
	case env, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: %s server disconnected", method, c.lang)
		}
		if env.Error != nil {
			return fmt.Errorf("%s: %s (code %d)", method, env.Error.Message, env.Error.Code)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(env.Result, out)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(callTimeout):
		return fmt.Errorf("%s: timed out after %s", method, callTimeout)
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	payload, err := json.Marshal(rpcNotification{JSONRPC: jsonRPCVersion, Method: method, Params: params})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.stream, payload)
}

// ensureOpen makes sure the server has the current on-disk content of
// path open under uri, per the v1 sync policy: no file watcher, no
// incremental didChange — every diagnostics/definition/references query
// simply (re)opens the file fresh. If it was already open from a previous
// query, it's closed first so the didOpen is spec-legal (a second
// didOpen for an already-open URI without an intervening didClose is
// undefined behavior per the spec).
func (c *Client) ensureOpen(ctx context.Context, uri, languageID, text string) error {
	c.docMu.Lock()
	wasOpen := c.docOpen[uri]
	c.docVer[uri]++
	version := c.docVer[uri]
	c.docMu.Unlock()

	if wasOpen {
		if err := c.notify(ctx, "textDocument/didClose", didCloseParams{TextDocument: textDocumentIdentifier{URI: uri}}); err != nil {
			return err
		}
	}

	// Diagnostics for this URI are about to go stale the moment we
	// (re)open it — drop the cached copy so a waiter doesn't get handed a
	// notification left over from the previous open.
	c.diagMu.Lock()
	delete(c.diagnostics, uri)
	c.diagMu.Unlock()

	if err := c.notify(ctx, "textDocument/didOpen", didOpenParams{TextDocument: textDocumentItem{
		URI: uri, LanguageID: languageID, Version: version, Text: text,
	}}); err != nil {
		return err
	}
	c.docMu.Lock()
	c.docOpen[uri] = true
	c.docMu.Unlock()
	return nil
}

// waitDiagnostics blocks until a publishDiagnostics notification arrives
// for uri, or timeout elapses — whichever first. On timeout it returns
// whatever is currently cached (possibly empty/stale) rather than an
// error: a clean file legitimately produces zero diagnostics and no
// notification may ever arrive to say so explicitly, and gopls in
// particular can take a few seconds after didOpen to finish analysis.
func (c *Client) waitDiagnostics(ctx context.Context, uri string, timeout time.Duration) []Diagnostic {
	c.diagMu.Lock()
	if d, ok := c.diagnostics[uri]; ok {
		c.diagMu.Unlock()
		return d
	}
	wait := make(chan struct{})
	c.diagWaiters[uri] = append(c.diagWaiters[uri], wait)
	c.diagMu.Unlock()

	select {
	case <-wait:
	case <-ctx.Done():
	case <-time.After(timeout):
	}

	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	return c.diagnostics[uri]
}

// definition runs textDocument/definition at a 0-indexed position.
func (c *Client) definition(ctx context.Context, uri string, pos Position) ([]Location, error) {
	var raw json.RawMessage
	if err := c.call(ctx, "textDocument/definition", textDocumentPositionParams{
		TextDocument: textDocumentIdentifier{URI: uri}, Position: pos,
	}, &raw); err != nil {
		return nil, err
	}
	return parseLocations(raw)
}

// references runs textDocument/references at a 0-indexed position.
func (c *Client) references(ctx context.Context, uri string, pos Position, includeDeclaration bool) ([]Location, error) {
	var raw json.RawMessage
	if err := c.call(ctx, "textDocument/references", referenceParams{
		TextDocument: textDocumentIdentifier{URI: uri}, Position: pos,
		Context: referenceContext{IncludeDeclaration: includeDeclaration},
	}, &raw); err != nil {
		return nil, err
	}
	return parseLocations(raw)
}

// Close shuts the server connection down. Safe to call more than once.
func (c *Client) Close() {
	c.closeOnce.Do(func() { _ = c.stream.Close() })
}
