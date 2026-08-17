package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/codexmark/kram-gateway/internal/openai"
)

// OpenAICompatible talks to any backend that already speaks the OpenAI
// chat-completions wire format: OpenAI itself, OpenRouter, opencode zen,
// and most other aggregators. Only base URL, API key and an optional
// pinned model differ between them.
type OpenAICompatible struct {
	id      string
	baseURL string
	apiKey  string
	model   string // optional: overrides req.Model when set
	client  *http.Client
}

// NewOpenAICompatible constructs an adapter for an OpenAI-shaped backend.
func NewOpenAICompatible(id, baseURL, apiKey, model string) *OpenAICompatible {
	return &OpenAICompatible{
		id:      id,
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *OpenAICompatible) ID() string   { return p.id }
func (p *OpenAICompatible) Kind() string { return "openai-compat" }

type openaiCompatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openai.Usage `json:"usage"`
}

func (p *OpenAICompatible) ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (<-chan StreamEvent, error) {
	model := req.Model
	if p.model != "" {
		model = p.model
	}
	body := req
	body.Model = model
	body.Stream = true

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", p.id, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", p.id, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", p.id, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("%s: upstream returned %s", p.id, resp.Status)
	}

	events := make(chan StreamEvent, 16)
	go func() {
		defer close(events)
		defer resp.Body.Close()

		var usage *openai.Usage
		err := scanSSEData(resp.Body, func(data string) bool {
			if data == "[DONE]" {
				return false
			}
			var chunk openaiCompatChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return true // skip malformed chunk, keep reading
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			for _, c := range chunk.Choices {
				if c.Delta.Content != "" {
					select {
					case events <- StreamEvent{Delta: c.Delta.Content}:
					case <-ctx.Done():
						return false
					}
				}
			}
			return true
		})
		if err != nil {
			events <- StreamEvent{Err: fmt.Errorf("%s: stream read: %w", p.id, err)}
			return
		}
		events <- StreamEvent{Done: true, Usage: usage}
	}()

	return events, nil
}
