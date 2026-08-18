// Package provider defines the common interface every upstream LLM backend
// implements, and the normalized streaming event shape the router/server
// work with regardless of which provider served the request.
package provider

import (
	"context"
	"fmt"

	"github.com/codexmark/kram-gateway/internal/openai"
)

// StreamEvent is one normalized increment of a chat completion stream.
// Providers translate their native wire format into a sequence of these.
// ToolCalls is only set on the final (Done) event — no provider streams
// partial tool-call deltas to Kram's own agent loop, which always makes
// non-streaming requests and waits for the complete decision before
// acting on it (see internal/daemon/agent).
type StreamEvent struct {
	Delta     string
	Done      bool
	Usage     *openai.Usage
	ToolCalls []openai.ToolCall
	Err       error
}

// Provider is anything that can serve a chat completion request.
type Provider interface {
	// ID is the stable identifier used in config, logs and telemetry.
	ID() string
	// Kind is the adapter family (anthropic, gemini, openai-compat) — useful
	// for diagnostics, not for routing decisions.
	Kind() string
	// SupportsImages and SupportsTools reflect the provider's configured
	// capabilities (internal/config.ProviderConfig) — callers must check
	// these before sending images or tool definitions.
	SupportsImages() bool
	SupportsTools() bool
	// ChatCompletion issues the request upstream and streams back normalized
	// events. The channel is always closed by the provider, exactly once,
	// after a final event with Done=true or Err set.
	ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (<-chan StreamEvent, error)
}

// capabilities is embedded by each adapter to implement the
// SupportsImages/SupportsTools half of Provider without repeating the same
// two fields and methods three times.
type capabilities struct {
	images bool
	tools  bool
}

func (c capabilities) SupportsImages() bool { return c.images }
func (c capabilities) SupportsTools() bool  { return c.tools }

// HTTPError wraps a non-2xx upstream response so a caller can report the
// real status code instead of just a formatted string — this is what lets
// the router's attempt executor tell an HTTP 500 apart from an HTTP 200
// whose *content* got rejected by the ResponseGate (see
// openai.AttemptInfo.HTTPStatus and DECISIONS.md).
type HTTPError struct {
	Provider   string
	StatusCode int
	Status     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: upstream returned %s", e.Provider, e.Status)
}
