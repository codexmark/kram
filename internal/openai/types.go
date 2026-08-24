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
	// ProviderItems carries opaque, replayable output state emitted by an
	// upstream provider. Today this is used by the Responses adapter for
	// encrypted reasoning items; other adapters ignore it. Keeping it on the
	// assistant turn lets store:false conversations retain reasoning continuity.
	ProviderItems []ProviderItem `json:"provider_items,omitempty"`
}

// ProviderItem is opaque provider state that must round-trip with an assistant
// turn. Only encrypted reasoning is accepted by the Responses adapter; the
// narrow shape prevents arbitrary response output from being replayed.
type ProviderItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"`
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
	// GeminiThoughtSignature is opaque provider-specific state Gemini's
	// thinking-enabled models attach to a function call — it must be
	// echoed back verbatim on this exact call when the conversation
	// continues, or Gemini rejects the request outright ("Function call
	// is missing a thought_signature"). Empty for every other provider,
	// and for Gemini responses from a non-thinking model. Kept on the
	// shared ToolCall (rather than a Gemini-only side channel) because
	// tool calls already round-trip through the daemon's session storage
	// and back out to whichever provider serves the next turn — the
	// value must survive that round-trip intact to be replayed, and
	// there's nowhere else in the pipeline that would preserve it.
	GeminiThoughtSignature string `json:"gemini_thought_signature,omitempty"`
}

// ToolCallFunction names the tool and carries its arguments as a raw JSON
// string (not a parsed object) — this matches every provider's actual wire
// format and avoids losing information to an intermediate schema.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// RunIDHeader is an optional HTTP request header a caller may send on
// POST /v1/chat/completions: an opaque identifier for one agent run (one
// user turn plus every tool round-trip it causes) — never one that
// persists across a later, unrelated turn in the same conversation. It's
// a kram-gateway extension carried out of band as a header rather than a
// JSON body field, so standard OpenAI-compatible clients that never send
// it stay fully compatible (see internal/router.RouteContext.RunKey and
// DECISIONS.md, "Sticky is run-scoped, not session-prefix-scoped").
const RunIDHeader = "X-Kram-Run-Id"

// PromptCacheKeyHeader groups calls from the same durable session for the
// upstream prompt cache. It is intentionally separate from RunIDHeader: run IDs
// change each user turn, while cache affinity should survive across turns.
const PromptCacheKeyHeader = "X-Kram-Prompt-Cache-Key"

// ChatCompletionRequest is the request body for POST /v1/chat/completions.
type ChatCompletionRequest struct {
	Model          string        `json:"model"`
	Messages       []ChatMessage `json:"messages"`
	Stream         bool          `json:"stream"`
	MaxTokens      *int          `json:"max_tokens,omitempty"`
	Temperature    *float64      `json:"temperature,omitempty"`
	Tools          []Tool        `json:"tools,omitempty"`
	PromptCacheKey string        `json:"-"`
}

// ChatCompletionChoice is one candidate answer in a non-streaming response.
type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage reports token accounting for a request.
type Usage struct {
	PromptTokens           int `json:"prompt_tokens"`
	CompletionTokens       int `json:"completion_tokens"`
	TotalTokens            int `json:"total_tokens"`
	CachedPromptTokens     int `json:"cached_prompt_tokens,omitempty"`
	CacheWritePromptTokens int `json:"cache_write_prompt_tokens,omitempty"`
	ReasoningTokens        int `json:"reasoning_tokens,omitempty"`
	// EstimatedCostMicros is list-price API-equivalent cost, not a charge to
	// a ChatGPT subscription. Zero means no trustworthy price is known.
	EstimatedCostMicros int64 `json:"estimated_cost_micros,omitempty"`
}

// AddUsage combines accounting from tool rounds, fallback candidates, or
// gateway retries without losing cache/reasoning/cost detail.
func AddUsage(a, b Usage) Usage {
	return Usage{
		PromptTokens:           a.PromptTokens + b.PromptTokens,
		CompletionTokens:       a.CompletionTokens + b.CompletionTokens,
		TotalTokens:            a.TotalTokens + b.TotalTokens,
		CachedPromptTokens:     a.CachedPromptTokens + b.CachedPromptTokens,
		CacheWritePromptTokens: a.CacheWritePromptTokens + b.CacheWritePromptTokens,
		ReasoningTokens:        a.ReasoningTokens + b.ReasoningTokens,
		EstimatedCostMicros:    a.EstimatedCostMicros + b.EstimatedCostMicros,
	}
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
	// Ranking is the full candidate ranking a scoring strategy produced
	// for this request, if any (nil for non-scoring strategies) — see
	// RankedProviderInfo.
	Ranking  []RankedProviderInfo `json:"ranking,omitempty"`
	Strategy string               `json:"strategy,omitempty"`
}

// AttemptOutcome classifies how one attempt ended — see AttemptInfo.
// Deliberately a small, closed set rather than free-form strings: the CLI
// switches on these to pick a glyph/color, and a routing decision (the
// gateway/router) is the only thing that should ever produce one.
type AttemptOutcome string

const (
	OutcomeTrying   AttemptOutcome = "trying"   // in flight — used only by live route.* events, never persisted on a finished attempt
	OutcomeSuccess  AttemptOutcome = "success"  // response received and accepted by the ResponseGate
	OutcomeError    AttemptOutcome = "error"    // transport/HTTP failure — never reached a response to gate
	OutcomeRejected AttemptOutcome = "rejected" // response received (HTTP 200 or equivalent) but the ResponseGate/StreamGate rejected it
	OutcomeSkipped  AttemptOutcome = "skipped"  // never attempted — filtered out before execution (circuit-open, capability mismatch)
)

// AttemptInfo records one provider attempt made while serving a request —
// the real fallback trail for a single completion. Combined with Outcome
// and Reason, this is what lets a caller tell an HTTP 500 apart from an
// HTTP 200 whose content the ResponseGate rejected — two very different
// situations that used to look identical (OK=false either way).
//
// OK is kept and still set (OK == Outcome == OutcomeSuccess) purely for
// backward compatibility: any code that only ever read the old two-field
// shape keeps working unchanged. New code should read Outcome.
type AttemptInfo struct {
	Provider  string `json:"provider"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`

	// Model is the upstream model ID actually requested for this attempt,
	// when known — providers can pin a model regardless of what the
	// client asked for (config.ProviderConfig.Model), so this can differ
	// from the request's own Model field.
	Model string `json:"model,omitempty"`
	// Outcome classifies how the attempt ended — see AttemptOutcome.
	Outcome AttemptOutcome `json:"outcome,omitempty"`
	// Reason is a short human-readable explanation, set whenever Outcome
	// isn't OutcomeSuccess: an upstream error message, a ResponseGate
	// rejection reason, or why a candidate was skipped.
	Reason string `json:"reason,omitempty"`
	// HTTPStatus is the upstream HTTP status code, when the attempt
	// reached one (0 for a transport-level failure that never got a
	// response, or for a skipped attempt).
	HTTPStatus int `json:"http_status,omitempty"`
	// Score is this candidate's ranking score from the strategy that
	// produced the routing order, when the strategy scores (nil for
	// non-scoring strategies like priority/round-robin). Pointer so "not
	// scored" and "scored zero" stay distinguishable.
	Score *float64 `json:"score,omitempty"`
	// Attempt is this attempt's 1-indexed position in the fallback chain
	// actually tried for this request (1 = first candidate tried).
	Attempt int `json:"attempt,omitempty"`
	// Class is this attempt's FailureClass, set whenever Outcome isn't
	// OutcomeSuccess — see Classify and ClassContentRejected's doc
	// comment for how a rejection (as opposed to a transport failure)
	// gets classified.
	Class FailureClass `json:"class,omitempty"`
	Usage *Usage       `json:"usage,omitempty"`
}

// ScoreFactor is one weighted component of a scoring strategy's decision
// for a single candidate — kept so a UI can show *why* a candidate scored
// the way it did without ever recomputing the score itself; it only ever
// renders what the router already decided (see DECISIONS.md).
type ScoreFactor struct {
	Name         string  `json:"name"`
	Weight       float64 `json:"weight"`       // 0..1, normalized
	Value        float64 `json:"value"`        // 0..1, this candidate's raw score for the factor
	Contribution float64 `json:"contribution"` // weight * value
}

// RankedProviderInfo is one entry in a scoring strategy's full ranking —
// distinct from AttemptInfo: every ranked candidate appears here whether
// or not it was actually attempted (fallback can stop before reaching a
// lower-ranked candidate), which is what lets a UI show "gemini .842,
// openai .816" as context even when only the top candidate was ever
// called.
type RankedProviderInfo struct {
	Provider string        `json:"provider"`
	Score    float64       `json:"score"`
	Factors  []ScoreFactor `json:"factors,omitempty"`
	Reasons  []string      `json:"reasons,omitempty"` // e.g. "sticky", "last-known-good", "cache-affinity"
}

// ChatCompletionChunkDelta is the incremental content of one SSE chunk. On
// the terminal chunk (FinishReason set), ToolCalls carries the fully
// assembled calls in one go rather than OpenAI's fragmented-by-index
// deltas — kram-gateway already does that reassembly per provider
// (internal/provider), and re-fragmenting it for a stream Kram itself is
// usually the only consumer of would just be extra work with no benefit.
type ChatCompletionChunkDelta struct {
	Role          string         `json:"role,omitempty"`
	Content       string         `json:"content,omitempty"`
	ToolCalls     []ToolCall     `json:"tool_calls,omitempty"`
	ProviderItems []ProviderItem `json:"provider_items,omitempty"`
}

// ChatCompletionChunkChoice wraps a delta inside a streaming chunk.
type ChatCompletionChunkChoice struct {
	Index        int                      `json:"index"`
	Delta        ChatCompletionChunkDelta `json:"delta"`
	FinishReason *string                  `json:"finish_reason"`
}

// ChatCompletionChunk is one `data: {...}` SSE event for streaming
// responses. Provider, Attempts and Usage are kram-gateway extensions,
// set only on the terminal chunk (mirrors ChatCompletionResponse) —
// used by the daemon's agent loop when a session opts into the
// streaming path (internal/daemon/agent.Config.PreferStreaming; buffered
// non-streaming calls, via ChatCompletionResponse instead, are the
// default — see that field's doc comment for why).
type ChatCompletionChunk struct {
	ID       string                      `json:"id"`
	Object   string                      `json:"object"`
	Created  int64                       `json:"created"`
	Model    string                      `json:"model"`
	Choices  []ChatCompletionChunkChoice `json:"choices"`
	Provider string                      `json:"provider,omitempty"`
	Attempts []AttemptInfo               `json:"attempts,omitempty"`
	Ranking  []RankedProviderInfo        `json:"ranking,omitempty"`
	Strategy string                      `json:"strategy,omitempty"`
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

	// Combo, Retryable, RetryAfterMS, Cause and Attempts are kram-gateway
	// extensions, populated only when every candidate in a combo failed
	// (see internal/server/chat.go's writeGatewayError) — a standard
	// OpenAI client ignores unknown fields; internal/daemon/gatewayclient
	// decodes them into a typed GatewayError instead of treating this as
	// a flat message string, so a caller can actually decide whether
	// retrying is worth it instead of guessing from prose. Combo being
	// non-empty is the signal this response came from kram-gateway's own
	// all-failed path, as opposed to some other OpenAI-compatible
	// server's plain error body.
	Combo string `json:"combo,omitempty"`
	// Retryable and Cause reflect the *last* attempt in the fallback
	// trail — the one that actually ended the request — not "any attempt
	// was retryable". A combo whose final candidate failed with a 400
	// isn't worth retrying even if an earlier candidate hit a 429.
	Retryable    bool          `json:"retryable,omitempty"`
	RetryAfterMS int64         `json:"retry_after_ms,omitempty"`
	Cause        FailureClass  `json:"cause,omitempty"`
	Attempts     []AttemptInfo `json:"attempts,omitempty"`
	Usage        Usage         `json:"usage,omitempty"`
}
