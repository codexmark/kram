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
	transport transport

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan message

	serverInfo string
	tools      []Tool
	closeOnce  sync.Once
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

	c := &Client{name: name, transport: tr, pending: make(map[int64]chan message)}
	go c.dispatch()

	if err := c.handshake(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("server %q: %w", name, err)
	}
	if err := c.loadTools(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("server %q: %w", name, err)
	}
	return c, nil
}

// dispatch routes each inbound message to whoever is waiting on its id.
// Server-initiated notifications (no id) are dropped: Kram advertises no
// capabilities that would make a server send one, and silently ignoring
// them is the spec-correct behavior for anything unrecognized.
func (c *Client) dispatch() {
	for msg := range c.transport.Recv() {
		if msg.ID == nil {
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
	c.serverInfo = res.ServerInfo.Name
	if res.ServerInfo.Version != "" {
		c.serverInfo += " " + res.ServerInfo.Version
	}
	return c.notify(ctx, "notifications/initialized", nil)
}

func (c *Client) loadTools(ctx context.Context) error {
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
		c.tools = append(c.tools, res.Tools...)
		if res.NextCursor == "" {
			return nil
		}
		cursor = res.NextCursor
	}
}

// Tools returns what this server advertised at connect time.
func (c *Client) Tools() []Tool { return c.tools }

// ServerInfo is the server's self-reported name and version, for logs.
func (c *Client) ServerInfo() string { return c.serverInfo }

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
