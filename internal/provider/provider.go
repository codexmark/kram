// Package provider defines the common interface every upstream LLM backend
// implements, and the normalized streaming event shape the router/server
// work with regardless of which provider served the request.
package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

// StreamEvent is one normalized increment of a chat completion stream.
// Providers translate their native wire format into a sequence of these.
// ToolCalls is only set on the final (Done) event — no provider streams
// partial tool-call deltas as assembled ToolCall values; the daemon's
// agent loop makes buffered (non-streaming) gateway calls by default and
// waits for the complete decision before acting on it (see
// internal/daemon/agent.Config.PreferStreaming for the opt-in exception),
// so there's nothing further upstream that needs them incrementally
// either. ToolCallProgress below exists for a narrower reason: not
// exposing partial arguments, just proving the provider is still alive.
type StreamEvent struct {
	Delta string
	// Reasoning is a fragment of a reasoning-capable model's chain-of-
	// thought, kept separate from Delta because it is not the model's
	// actual answer — a caller must never relay it as assistant message
	// content. It exists purely as a liveness signal: a provider that's
	// only produced reasoning so far is actively working, not stalled,
	// which router.BoundedPeek needs to know (see DECISIONS.md — a
	// reasoning phase alone routinely runs past what used to be a fixed
	// peek timeout, and was being misread as a dead attempt).
	Reasoning string
	// ToolCallProgress marks an event that carried tool-call argument
	// fragments but no answer content and no reasoning — real evidence
	// the provider is actively producing tokens, exactly like Reasoning
	// is, but kept as a separate bool rather than folded into it: a
	// caller must never mistake tool-call JSON fragments for chain-of-
	// thought text. Exists purely as a liveness signal for
	// router.BoundedPeek — a provider streaming a long tool call's
	// arguments token-by-token, with no leading text, used to look like
	// dead silence and get killed as stalled even though it was actively
	// working the whole time (every adapter accumulates tool-call
	// fragments internally without emitting any StreamEvent at all,
	// unless this is set).
	ToolCallProgress bool
	Done             bool
	Usage            *openai.Usage
	ToolCalls        []openai.ToolCall
	Err              error
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
	// RetryAfter is parsed from the upstream response's Retry-After
	// header (seconds form only — the rarer HTTP-date form isn't worth
	// the parsing complexity for the providers Kram actually talks to).
	// Zero when absent or unparseable; a caller deciding on a Gateway
	// Round retry then falls back to its own computed backoff.
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: upstream returned %s", e.Provider, e.Status)
}
