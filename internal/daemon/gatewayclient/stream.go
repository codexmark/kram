package gatewayclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/codexmark/kram-gateway/internal/openai"
)

// StreamDelta is one normalized increment of a streaming chat completion —
// either a text fragment, or (on the final delta, Done set) the complete
// picture the non-streaming Result carries: which provider served it, the
// fallback trail, usage, and any tool calls the model is requesting.
type StreamDelta struct {
	Content   string
	Done      bool
	ToolCalls []openai.ToolCall
	Provider  string
	Usage     openai.Usage
	Attempts  []openai.AttemptInfo
	Err       error
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

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling gateway: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var errResp openai.ErrorResponse
		if json.NewDecoder(resp.Body).Decode(&errResp) == nil && errResp.Error.Message != "" {
			return nil, fmt.Errorf("gateway error: %s", errResp.Error.Message)
		}
		return nil, fmt.Errorf("gateway returned %s", resp.Status)
	}

	out := make(chan StreamDelta, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
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
			}

			if choice.FinishReason != nil {
				delta := StreamDelta{Done: true, ToolCalls: choice.Delta.ToolCalls, Provider: chunk.Provider, Attempts: chunk.Attempts}
				if chunk.Usage != nil {
					delta.Usage = *chunk.Usage
				}
				if *choice.FinishReason == "error" {
					delta.Err = fmt.Errorf("%s: stream ended in error", chunk.Provider)
				}
				select {
				case out <- delta:
				case <-ctx.Done():
				}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case out <- StreamDelta{Err: fmt.Errorf("reading gateway stream: %w", err)}:
			case <-ctx.Done():
			}
		}
	}()

	return out, nil
}
