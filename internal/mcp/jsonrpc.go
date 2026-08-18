// Package mcp is a from-scratch Model Context Protocol client — the way
// Kram reaches third-party tool servers (filesystem, GitHub, databases,
// whatever someone has published) instead of only the tools compiled into
// its own binary. No SDK dependency: MCP is JSON-RPC 2.0 over stdio or
// HTTP, and hand-rolling it keeps Kram's "pure-Go, static binary, no
// surprise dependencies" property.
//
// Protocol era: this implements the *stateful* lifecycle (`initialize` →
// `notifications/initialized` → `tools/list`/`tools/call`) used by
// revisions through 2025-11-25. The 2026-07-28 revision removes the
// handshake entirely in favor of stateless per-request metadata; that's
// deliberately deferred, because essentially every MCP server actually
// deployed today speaks the older lifecycle. See handshake() for where a
// modern-era probe would slot in.
package mcp

import "encoding/json"

const jsonRPCVersion = "2.0"

// protocolVersion is the revision Kram advertises during initialize. A
// server may answer with a different one; anything it reports that we
// don't recognize is accepted anyway, since tools/list and tools/call
// have been wire-stable across every revision that has the handshake.
const protocolVersion = "2025-11-25"

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// message is the inbound shape: a response (Result or Error, with ID) or
// a server-initiated notification (Method, no ID). One struct for both
// because they arrive interleaved on the same channel and the reader
// can't know which it has until it's parsed.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// --- initialize ---

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions,omitempty"`
}

// --- tools ---

// Tool is one tool a server exposes. InputSchema is passed through to the
// LLM provider untouched — it's already JSON Schema, which is exactly
// what a tool definition needs.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type listToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type callToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

// --- resources ---

// Resource is one piece of server-exposed readable data, identified by
// URI — a file, a database row, whatever the server wants to expose.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type listResourcesResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

type readResourceParams struct {
	URI string `json:"uri"`
}

type resourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"` // base64 binary — surfaced as a placeholder, not decoded
}

type readResourceResult struct {
	Contents []resourceContent `json:"contents"`
}

// --- prompts ---

// Prompt is a server-defined, parameterized prompt template.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type listPromptsResult struct {
	Prompts    []Prompt `json:"prompts"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

type getPromptParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

type promptMessage struct {
	Role    string `json:"role"`
	Content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type getPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []promptMessage `json:"messages"`
}

// contentBlock covers the block types a tool result can carry. Kram's
// tool protocol is text-only (every tool returns a string), so non-text
// blocks are rendered as a short placeholder rather than dropped
// silently — the model should know an image came back even if it can't
// see it through this path.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Resource *struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
	} `json:"resource,omitempty"`
}
