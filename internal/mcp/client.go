package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// callTimeout bounds a single JSON-RPC round-trip. MCP servers are
// third-party processes Kram doesn't control; without this, one hung
// server would park an agent turn until the whole run's context expires.
const callTimeout = 60 * time.Second

// Client is a connected MCP server: one transport, one pending-request
// table, and the tools the server advertised.
type Client struct {
	name      string
	cfg       ServerConfig // retained for cache fingerprinting and reconnect
	transport transport

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan message

	// toolsMu guards serverInfo/tools independently of mu: a
	// tools/list_changed refresh runs call() (which briefly takes mu to
	// register a pending id) from its own goroutine, so tying the tools
	// snapshot to the same lock would mean readers of Tools() contend with
	// in-flight request bookkeeping for no reason.
	toolsMu    sync.RWMutex
	serverInfo string
	tools      []Tool

	// done closes once dispatch() observes the transport's Recv() channel
	// close — the signal a Manager watches to notice this client died and
	// start reconnecting. Closed exactly once, by dispatch() itself.
	done      chan struct{}
	closeOnce sync.Once
}

// Connect starts (or dials) one configured server, performs the
// handshake, and lists its tools. A server that fails any of that is
// returned as an error and simply won't contribute tools — one broken
// entry in a config must never stop the daemon from starting.
func Connect(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
	var (
		tr  transport
		err error
	)
	switch cfg.kind() {
	case kindStdio:
		tr, err = newStdioTransport(ctx, cfg.Command, cfg.Args, cfg.Env)
		if err != nil {
			return nil, err
		}
	case kindHTTP:
		tr = newHTTPTransport(cfg.URL, cfg.Headers)
	default:
		return nil, fmt.Errorf("server %q: needs either command (stdio) or url (http)", name)
	}

	c := &Client{name: name, cfg: cfg, transport: tr, pending: make(map[int64]chan message), done: make(chan struct{})}
	go c.dispatch()

	if err := c.handshake(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("server %q: %w", name, err)
	}
	if err := c.loadTools(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("server %q: %w", name, err)
	}
	// Best-effort: a fresh, real tools/list is exactly what the schema
	// cache exists to remember. Saving here (not just after a later
	// refresh) means even a daemon that's never seen a tools/list_changed
	// notification still has an up-to-date cache for diagnostics.
	saveCacheEntry(name, cfg, c.ServerInfo(), c.Tools())
	return c, nil
}

// dispatch routes each inbound message to whoever is waiting on its id.
// Server-initiated notifications (no id) are handled by handleNotification
// — today that means recognizing tools/list_changed and refreshing the
// tool snapshot; anything else is dropped, which is still the
// spec-correct behavior for a notification Kram doesn't act on.
func (c *Client) dispatch() {
	for msg := range c.transport.Recv() {
		if msg.ID == nil {
			c.handleNotification(msg)
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if ok {
			ch <- msg
		}
	}
	// Transport closed — unblock anything still waiting rather than
	// leaving those callers parked until their own timeout.
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	// Signal the client is dead — the Manager's supervisor goroutine (if
	// any) is watching this to know it's time to start reconnecting.
	close(c.done)
}

// handleNotification reacts to a server-initiated message. Only
// tools/list_changed is recognized; everything else is exactly the
// silent-drop behavior this client has always had for unrecognized
// notifications.
func (c *Client) handleNotification(msg message) {
	if msg.Method != "notifications/tools/list_changed" {
		return
	}
	// Refresh in its own goroutine: dispatch() must keep pumping the
	// transport's Recv() channel, and tools/list is itself a call() that
	// goes back out over this same transport and waits for a reply.
	go c.refreshTools()
}

// refreshTools re-fetches tools/list after a tools/list_changed
// notification and atomically swaps the client's snapshot. This
// deliberately does NOT touch anything already in flight: the agent loop
// reads tool definitions once at the start of each turn (see
// tools.Registry.Definitions, called from daemon/agent/agent.go before
// each model call, never mid-call), so swapping the snapshot here — at
// any point between turns — can never retroactively change a request
// that's already been sent to a provider. It only changes what the next
// call to Tools() (and therefore the next turn) sees.
func (c *Client) refreshTools() {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := c.loadTools(ctx); err != nil {
		// Best-effort: keep serving the previous snapshot rather than
		// discarding it over a refresh that failed (server hiccup, timeout).
		return
	}
	saveCacheEntry(c.name, c.cfg, c.ServerInfo(), c.Tools())
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(request{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if err := c.transport.Send(ctx, payload); err != nil {
		return fmt.Errorf("sending %s: %w", method, err)
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: server disconnected", method)
		}
		if msg.Error != nil {
			return fmt.Errorf("%s: %s (code %d)", method, msg.Error.Message, msg.Error.Code)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(callTimeout):
		return fmt.Errorf("%s: timed out after %s", method, callTimeout)
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	payload, err := json.Marshal(notification{JSONRPC: jsonRPCVersion, Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.transport.Send(ctx, payload)
}

// handshake runs the stateful lifecycle: initialize, then the
// initialized notification. (A 2026-07-28-era client would instead probe
// `server/discover` here and skip the handshake entirely when the server
// answers it — see the package doc for why that's deferred.)
func (c *Client) handshake(ctx context.Context) error {
	var res initializeResult
	err := c.call(ctx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: "kram", Version: "0.1"},
	}, &res)
	if err != nil {
		return err
	}
	info := res.ServerInfo.Name
	if res.ServerInfo.Version != "" {
		info += " " + res.ServerInfo.Version
	}
	c.toolsMu.Lock()
	c.serverInfo = info
	c.toolsMu.Unlock()
	return c.notify(ctx, "notifications/initialized", nil)
}

// loadTools fetches every page of tools/list and replaces the client's
// tool snapshot with the result. Used both for the initial connect and
// for a tools/list_changed-triggered refresh — either way it's a full
// replace, not a merge, since a server that renamed or removed a tool
// should have that reflected too, not just additions picked up.
func (c *Client) loadTools(ctx context.Context) error {
	var tools []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res listToolsResult
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return err
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	c.toolsMu.Lock()
	c.tools = tools
	c.toolsMu.Unlock()
	return nil
}

// Tools returns the server's most recently known tool list — what it
// advertised at connect time, or what a later tools/list_changed
// notification refreshed it to, whichever is newer. Safe to call
// concurrently with a refresh in progress.
func (c *Client) Tools() []Tool {
	c.toolsMu.RLock()
	defer c.toolsMu.RUnlock()
	return c.tools
}

// ServerInfo is the server's self-reported name and version, for logs.
func (c *Client) ServerInfo() string {
	c.toolsMu.RLock()
	defer c.toolsMu.RUnlock()
	return c.serverInfo
}

// Done closes once this client's transport has gone away — dispatch()
// noticing Recv() close, whether because the server process died, the
// connection dropped, or Close() was called deliberately. A Manager uses
// this to know when to start reconnecting a specific server.
func (c *Client) Done() <-chan struct{} { return c.done }

// CallTool invokes one tool and flattens the result into plain text,
// which is what Kram's tool protocol carries. A tool-level failure
// (isError) comes back as text rather than a Go error on purpose: the
// agent loop feeds it straight back to the model, which can then read
// what went wrong and correct itself — the same contract Kram's built-in
// tools follow.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var res callToolResult
	err := c.call(ctx, "tools/call", callToolParams{Name: name, Arguments: args}, &res)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if res.IsError {
		b.WriteString("error: ")
	}
	for i, block := range res.Content {
		if i > 0 {
			b.WriteString("\n")
		}
		switch block.Type {
		case "text":
			b.WriteString(block.Text)
		case "resource":
			if block.Resource != nil && block.Resource.Text != "" {
				b.WriteString(block.Resource.Text)
			} else if block.Resource != nil {
				fmt.Fprintf(&b, "(resource: %s)", block.Resource.URI)
			}
		case "resource_link":
			fmt.Fprintf(&b, "(resource link: %s)", block.URI)
		default:
			// image/audio and anything newer: Kram's tool results are
			// text, so say what arrived instead of dropping it silently.
			fmt.Fprintf(&b, "(%s content, %s — not renderable in a text tool result)", block.Type, block.MimeType)
		}
	}
	if b.Len() == 0 {
		return "(empty result)", nil
	}
	return b.String(), nil
}

// ListResources fetches every resource this server currently exposes.
// Unlike Tools (cached at connect time), this is fetched fresh on every
// call — a server's resource list can be large or change, and Kram has
// no reason to hold a stale copy of it in memory between calls.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	var out []Resource
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res listResourcesResult
		if err := c.call(ctx, "resources/list", params, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Resources...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// ReadResource fetches one resource's content by URI and flattens it to
// text, same convention CallTool uses — a binary resource (Blob) is
// reported as a placeholder rather than decoded, since Kram's tool
// results are text-only end to end.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	var res readResourceResult
	if err := c.call(ctx, "resources/read", readResourceParams{URI: uri}, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, content := range res.Contents {
		if i > 0 {
			b.WriteString("\n")
		}
		if content.Text != "" {
			b.WriteString(content.Text)
		} else if content.Blob != "" {
			fmt.Fprintf(&b, "(binary resource, %s — not renderable as text)", content.MimeType)
		}
	}
	if b.Len() == 0 {
		return "(empty resource)", nil
	}
	return b.String(), nil
}

// ListPrompts fetches every prompt template this server currently
// exposes, same on-demand-not-cached reasoning as ListResources.
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	var out []Prompt
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res listPromptsResult
		if err := c.call(ctx, "prompts/list", params, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Prompts...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// GetPrompt resolves one named prompt template with the given arguments
// and flattens its messages to text — Kram surfaces this as a tool
// result for the model to read and act on, not as literal conversation
// messages injected into history.
func (c *Client) GetPrompt(ctx context.Context, name string, arguments map[string]string) (string, error) {
	var res getPromptResult
	if err := c.call(ctx, "prompts/get", getPromptParams{Name: name, Arguments: arguments}, &res); err != nil {
		return "", err
	}
	var b strings.Builder
	if res.Description != "" {
		b.WriteString(res.Description + "\n\n")
	}
	for _, m := range res.Messages {
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content.Text)
	}
	return b.String(), nil
}

// Close shuts the server down.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { _ = c.transport.Close() })
	return nil
}
