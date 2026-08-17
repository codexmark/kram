// Package gatewayclient is the daemon's HTTP client for kram-gateway. The
// daemon never talks to LLM providers directly — every model call goes
// through the gateway, so load balancing, fallback and (later) compression
// apply uniformly regardless of which client drove the session.
package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/codexmark/kram-gateway/internal/openai"
)

// Client calls a kram-gateway instance's OpenAI-compatible API.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a client pointed at a running kram-gateway (e.g. http://127.0.0.1:20128).
func New(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 120 * time.Second}}
}

// ChatCompletion sends a non-streaming chat completion request and returns
// the assistant's reply content and the model name that actually served it.
func (c *Client) ChatCompletion(ctx context.Context, model string, messages []openai.ChatMessage) (string, error) {
	req := openai.ChatCompletionRequest{Model: model, Messages: messages, Stream: false}
	payload, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encoding gateway request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("building gateway request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("calling gateway: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp openai.ErrorResponse
		if json.NewDecoder(resp.Body).Decode(&errResp) == nil && errResp.Error.Message != "" {
			return "", fmt.Errorf("gateway error: %s", errResp.Error.Message)
		}
		return "", fmt.Errorf("gateway returned %s", resp.Status)
	}

	var completion openai.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return "", fmt.Errorf("decoding gateway response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("gateway response had no choices")
	}
	return completion.Choices[0].Message.Content, nil
}
