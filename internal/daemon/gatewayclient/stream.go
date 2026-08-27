package gatewayclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

// StreamDelta is one normalized increment of a streaming chat completion —
// either a text fragment, or (on the final delta, Done set) the complete
// picture the non-streaming Result carries: which provider served it, the
// fallback trail, usage, and any tool calls the model is requesting.
type StreamDelta struct {
	Content string
	// Reasoning mirrors openai.ChatCompletionChunkDelta.Reasoning — a
	// best-effort chain-of-thought fragment, never set alongside Content
	// on the same delta. Most providers never send it.
	Reasoning     string
	Done          bool
	ToolCalls     []openai.ToolCall
	ProviderItems []openai.ProviderItem
	Provider      string
	Usage         openai.Usage
	Attempts      []openai.AttemptInfo
	// Ranking and Strategy mirror Result's — see its doc comment.
	Ranking  []openai.RankedProviderInfo
	Strategy string
	Err      error
}

// ChatCompletionStream is ChatCompletion's streaming counterpart: same
// request shape, but the gateway's response is relayed chunk by chunk
// instead of buffered. The daemon's agent loop runs entirely off this —
// tool-call turns are silent (no user-visible deltas until Done reveals
// ToolCalls), text-answering turns stream live to whoever is reading the
// channel.
func (c *Client) ChatCompletionStream(ctx context.Context, model string, messages []openai.ChatMessage, tools []openai.Tool) (<-chan StreamDelta, error) {
	req := openai.ChatCompletionRequest{Model: model, Messages: messages, Stream: true, Tools: tools}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding gateway request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building gateway request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if runID := runIDFromContext(ctx); runID != "" {
		httpReq.Header.Set(openai.RunIDHeader, runID)
	}
	if key := promptCacheKeyFromContext(ctx); key != "" {
		httpReq.Header.Set(openai.PromptCacheKeyHeader, key)
	}

	// Phase liveness instead of a whole-call cap: c.timeout bounds
	// connect+headers, then converts into an idle backstop reset by every
	// byte read — the gateway's keep-alive comments (internal/server's
	// streamResponse) keep it fresh while an upstream attempt is alive but
	// quiet, so this only ever fires when the pipe is genuinely dead. A
	// whole-call cap here used to kill healthy long generations one hop
	// above the provider adapters' identical bug.
	wctx, cancel := context.WithCancel(ctx)
	var watchdogFired atomic.Bool
	watchdog := time.AfterFunc(c.timeout, func() {
		watchdogFired.Store(true)
		cancel()
	})
	stopWatchdog := func() {
		watchdog.Stop()
		cancel()
	}
	httpReq = httpReq.WithContext(wctx)

	resp, err := c.stream.Do(httpReq)
	if err != nil {
		stopWatchdog()
		if watchdogFired.Load() {
			err = fmt.Errorf("no data from the gateway for %s (idle timeout)", c.timeout)
		}
		return nil, fmt.Errorf("calling gateway: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer stopWatchdog()
		defer resp.Body.Close()
		var errResp openai.ErrorResponse
		if json.NewDecoder(resp.Body).Decode(&errResp) == nil && errResp.Error.Message != "" {
			// Typed like the buffered path: an all-candidates-failed round
			// must reach the agent's Gateway Round retry as a GatewayError
			// (Retryable, RetryAfter) — a flat string here silently disabled
			// retries for every streaming session.
			if ge := gatewayErrorFromBody(errResp); ge != nil {
				return nil, ge
			}
			return nil, fmt.Errorf("gateway error: %s", errResp.Error.Message)
		}
		return nil, fmt.Errorf("gateway returned %s", resp.Status)
	}
	body := &idleWatchdogBody{rc: resp.Body, timer: watchdog, idle: c.timeout, fired: &watchdogFired}

	out := make(chan StreamDelta, 16)
	go func() {
		defer close(out)
		defer stopWatchdog()
		defer resp.Body.Close()

		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				return
			}

			var chunk openai.ChatCompletionChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue // skip malformed chunk, keep reading
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]

			if choice.Delta.Content != "" {
				select {
				case out <- StreamDelta{Content: choice.Delta.Content}:
				case <-ctx.Done():
					return
				}
			} else if choice.Delta.Reasoning != "" {
				select {
				case out <- StreamDelta{Reasoning: choice.Delta.Reasoning}:
				case <-ctx.Done():
					return
				}
			}

			if choice.FinishReason != nil {
				delta := StreamDelta{
					Done: true, ToolCalls: choice.Delta.ToolCalls, ProviderItems: choice.Delta.ProviderItems, Provider: chunk.Provider,
					Attempts: chunk.Attempts, Ranking: chunk.Ranking, Strategy: chunk.Strategy,
				}
				if chunk.Usage != nil {
					delta.Usage = *chunk.Usage
				}
				if *choice.FinishReason == "error" {
					// A committed stream that died mid-answer. The terminal
					// chunk carries the real attempt trail, so build the same
					// typed GatewayError the pre-commit paths produce — this
					// is what lets the agent's retry rounds resume a dropped
					// answer instead of failing the whole turn.
					retryable := false
					var cause openai.FailureClass
					for _, a := range chunk.Attempts {
						if a.Class.Retryable() {
							retryable = true
						}
						cause = a.Class
					}
					ge := &GatewayError{
						Retryable: retryable, Cause: cause, Attempts: chunk.Attempts,
						Message: fmt.Sprintf("%s: stream ended in error", chunk.Provider),
					}
					if chunk.Usage != nil {
						ge.Usage = *chunk.Usage
					}
					delta.Err = ge
				}
				select {
				case out <- delta:
				case <-ctx.Done():
				}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			if watchdogFired.Load() {
				err = fmt.Errorf("no data from the gateway for %s (idle timeout)", c.timeout)
			}
			select {
			case out <- StreamDelta{Err: fmt.Errorf("reading gateway stream: %w", err)}:
			case <-ctx.Done():
			}
		}
	}()

	return out, nil
}

// idleWatchdogBody resets the streaming watchdog on every successful read,
// so the timer measures quiet stretches of the gateway stream rather than
// its total duration — the counterpart of internal/provider's watchdogBody
// one hop down, duplicated here rather than shared for the same layering
// reason internal/cli/app duplicates oauthRefreshAdapter.
type idleWatchdogBody struct {
	rc    io.ReadCloser
	timer *time.Timer
	idle  time.Duration
	fired *atomic.Bool
}

func (b *idleWatchdogBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, err
}
