// Package gatewayclient is the daemon's HTTP client for kram-gateway. The
// daemon never talks to LLM providers directly — every model call goes
// through the gateway, so load balancing, fallback and compression apply
// uniformly regardless of which client drove the session.
package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

// GatewayError is what ChatCompletion returns when the gateway itself
// reports every candidate in a combo failed — the typed, daemon-side
// counterpart of openai.ErrorBody's extension fields, so a caller
// (internal/daemon/agent's Gateway Round retry) can decide whether
// retrying is worth it instead of pattern-matching an error string.
type GatewayError struct {
	Combo      string
	Retryable  bool
	RetryAfter time.Duration
	Cause      openai.FailureClass
	Attempts   []openai.AttemptInfo
	Message    string
	Usage      openai.Usage
}

func (e *GatewayError) Error() string { return e.Message }

// Client calls a kram-gateway instance's OpenAI-compatible API.
type Client struct {
	baseURL string
	http    *http.Client
}

// DefaultTimeout is the whole-call timeout New uses. It's a floor, not a
// ceiling for real deployments: because one gateway call may walk a fallback
// chain of several providers, the daemon derives a larger, chain-coherent
// value (see config.Tunables.ResolvedGatewayClientTimeout) and passes it to
// NewWithTimeout. A fixed 180s here is smaller than 2×the 120s provider
// timeout, so it must never be the client's real limit when a combo has more
// than one provider — otherwise a healthy multi-candidate fallback round
// could be cut off client-side before the chain is exhausted.
const DefaultTimeout = 180 * time.Second

// New builds a client pointed at a running kram-gateway (e.g. http://127.0.0.1:20128)
// with the default whole-call timeout.
func New(baseURL string) *Client {
	return NewWithTimeout(baseURL, DefaultTimeout)
}

// NewWithTimeout is New with an explicit whole-call timeout; a non-positive
// timeout falls back to DefaultTimeout.
func NewWithTimeout(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: timeout}}
}

type ctxKey int

const (
	runIDCtxKey ctxKey = iota
	promptCacheKeyCtxKey
)

// WithRunID attaches an opaque per-agent-run identifier to ctx. Every
// gateway call made with the returned context sends it as
// openai.RunIDHeader, which is what lets kram-gateway's Sticky routing
// tell one agent run's tool round-trips apart from a later, unrelated
// turn in the same session — see DECISIONS.md, "Sticky is run-scoped,
// not session-prefix-scoped". The agent loop calls this once per
// Service.Run/RunTask; nothing else needs to.
func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDCtxKey, runID)
}

func runIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(runIDCtxKey).(string)
	return id
}

func WithPromptCacheKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, promptCacheKeyCtxKey, key)
}

func promptCacheKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(promptCacheKeyCtxKey).(string)
	return key
}

// Result is what a chat completion call actually produced: the reply
// (text and/or tool calls), which provider served it, the real fallback
// trail attempted to get there, and token usage for that request.
type Result struct {
	Content   string
	ToolCalls []openai.ToolCall
	Provider  string
	Attempts  []openai.AttemptInfo
	// Ranking and Strategy are the router's own full candidate ranking
	// and the combo's configured strategy name for this call — kram-
	// gateway extensions passed straight through from
	// openai.ChatCompletionResponse, used to build a RouteCall (see
	// internal/daemon/agent/route.go).
	Ranking       []openai.RankedProviderInfo
	Strategy      string
	Usage         openai.Usage
	ProviderItems []openai.ProviderItem
}

// ChatCompletion sends a non-streaming chat completion request — with the
// full message history and, if the caller wants tool calling, the tool
// definitions to offer the model — and returns whatever the gateway's
// chosen provider produced.
func (c *Client) ChatCompletion(ctx context.Context, model string, messages []openai.ChatMessage, tools []openai.Tool) (Result, error) {
	req := openai.ChatCompletionRequest{Model: model, Messages: messages, Stream: false, Tools: tools}
	payload, err := json.Marshal(req)
	if err != nil {
		return Result{}, fmt.Errorf("encoding gateway request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Result{}, fmt.Errorf("building gateway request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if runID := runIDFromContext(ctx); runID != "" {
		httpReq.Header.Set(openai.RunIDHeader, runID)
	}
	if key := promptCacheKeyFromContext(ctx); key != "" {
		httpReq.Header.Set(openai.PromptCacheKeyHeader, key)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("calling gateway: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp openai.ErrorResponse
		if json.NewDecoder(resp.Body).Decode(&errResp) == nil && errResp.Error.Message != "" {
			// Combo is non-empty only for kram-gateway's own all-failed
			// response (see internal/server/chat.go's writeGatewayError) —
			// any other OpenAI-compatible server's plain error body falls
			// through to the flat error below, so ChatCompletion keeps
			// working against a non-Kram endpoint too.
			if errResp.Error.Combo != "" {
				return Result{}, &GatewayError{
					Combo: errResp.Error.Combo, Retryable: errResp.Error.Retryable,
					RetryAfter: time.Duration(errResp.Error.RetryAfterMS) * time.Millisecond,
					Cause:      errResp.Error.Cause, Attempts: errResp.Error.Attempts,
					Usage:   errResp.Error.Usage,
					Message: fmt.Sprintf("gateway error: %s", errResp.Error.Message),
				}
			}
			return Result{}, fmt.Errorf("gateway error: %s", errResp.Error.Message)
		}
		return Result{}, fmt.Errorf("gateway returned %s", resp.Status)
	}

	var completion openai.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return Result{}, fmt.Errorf("decoding gateway response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return Result{}, fmt.Errorf("gateway response had no choices")
	}
	return Result{
		Content:       completion.Choices[0].Message.Content,
		ToolCalls:     completion.Choices[0].Message.ToolCalls,
		Provider:      completion.Provider,
		Attempts:      completion.Attempts,
		Ranking:       completion.Ranking,
		Strategy:      completion.Strategy,
		Usage:         completion.Usage,
		ProviderItems: completion.Choices[0].Message.ProviderItems,
	}, nil
}
