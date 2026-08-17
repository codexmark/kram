// Package openai defines the OpenAI-compatible wire format that kram-gateway
// exposes to clients, regardless of which upstream provider actually serves
// the request.
package openai

import "encoding/json"

// ChatMessage is a single turn in a chat completion request. Role is one of
// "system", "user", "assistant", or "tool". An assistant message that wants
// to call tools sets ToolCalls instead of (or alongside) Content; a "tool"
// message reports one tool's result and must set ToolCallID.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// Images are data: URLs (kram extension, not standard OpenAI wire
	// format) — kept separate from Content instead of OpenAI's
	// content-parts array so Content can stay a plain string everywhere
	// else in the codebase.
	Images []string `json:"images,omitempty"`
	// ToolCalls is set on an assistant message that is requesting one or
	// more tool invocations instead of (or before) answering in text.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID and Name identify which call a role:"tool" message answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// Tool is one function the model may call, described as JSON Schema —
// the same shape OpenAI's API uses, since every provider adapter already
// has to translate to/from its own native tool format anyway.
type Tool struct {
	Type     string       `json:"type"` // always "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable tool's name, purpose, and argument shape.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema object
}

// ToolCall is one invocation the model is requesting.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction names the tool and carries its arguments as a raw JSON
// string (not a parsed object) — this matches every provider's actual wire
// format and avoids losing information to an intermediate schema.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatCompletionRequest is the request body for POST /v1/chat/completions.
type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Tools       []Tool        `json:"tools,omitempty"`
}

// ChatCompletionChoice is one candidate answer in a non-streaming response.
type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage reports token accounting for a request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionResponse is the non-streaming response body.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   Usage                  `json:"usage"`
	// Provider and Attempts are kram-gateway extensions (ignored by
	// standard OpenAI clients): which upstream actually served the
	// request, and the full fallback trail attempted to get there.
	Provider string        `json:"provider,omitempty"`
	Attempts []AttemptInfo `json:"attempts,omitempty"`
}

// AttemptInfo records one provider attempt made while serving a request,
// successful or not — the real fallback trail for a single completion.
type AttemptInfo struct {
	Provider  string `json:"provider"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
}

// ChatCompletionChunkDelta is the incremental content of one SSE chunk. On
// the terminal chunk (FinishReason set), ToolCalls carries the fully
// assembled calls in one go rather than OpenAI's fragmented-by-index
// deltas — kram-gateway already does that reassembly per provider
// (internal/provider), and re-fragmenting it for a stream Kram itself is
// usually the only consumer of would just be extra work with no benefit.
type ChatCompletionChunkDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ChatCompletionChunkChoice wraps a delta inside a streaming chunk.
type ChatCompletionChunkChoice struct {
	Index        int                      `json:"index"`
	Delta        ChatCompletionChunkDelta `json:"delta"`
	FinishReason *string                  `json:"finish_reason"`
}

// ChatCompletionChunk is one `data: {...}` SSE event for streaming
// responses. Provider, Attempts and Usage are kram-gateway extensions,
// set only on the terminal chunk (mirrors ChatCompletionResponse) — the
// daemon relies on these to run its agent loop entirely off the
// streaming path instead of needing a separate non-streaming call.
type ChatCompletionChunk struct {
	ID       string                      `json:"id"`
	Object   string                      `json:"object"`
	Created  int64                       `json:"created"`
	Model    string                      `json:"model"`
	Choices  []ChatCompletionChunkChoice `json:"choices"`
	Provider string                      `json:"provider,omitempty"`
	Attempts []AttemptInfo               `json:"attempts,omitempty"`
	Usage    *Usage                      `json:"usage,omitempty"`
}

// ErrorResponse is the OpenAI-compatible error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the error message and classification.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
