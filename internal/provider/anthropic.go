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

const defaultAnthropicBaseURL = "https://api.anthropic.com"

// Anthropic talks to the native Messages API (x-api-key auth, "system" is a
// top-level field rather than a message role, streaming uses named SSE
// events instead of OpenAI's flat "data:" chunks).
type Anthropic struct {
	id      string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewAnthropic constructs the Anthropic adapter. baseURL defaults to the
// public API when empty.
func NewAnthropic(id, baseURL, apiKey, model string) *Anthropic {
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}
	return &Anthropic{
		id:      id,
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *Anthropic) ID() string   { return p.id }
func (p *Anthropic) Kind() string { return "anthropic" }

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
}

type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Message struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func (p *Anthropic) ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (<-chan StreamEvent, error) {
	model := req.Model
	if p.model != "" {
		model = p.model
	}

	var system string
	messages := make([]anthropicMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = m.Content
			continue
		}
		messages = append(messages, anthropicMessage{Role: m.Role, Content: m.Content})
	}

	maxTokens := 4096
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}

	body := anthropicRequest{
		Model:     model,
		System:    system,
		Messages:  messages,
		MaxTokens: maxTokens,
		Stream:    true,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", p.id, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", p.id, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
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

		var inputTokens, outputTokens int
		err := scanSSEData(resp.Body, func(data string) bool {
			var evt anthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				return true // skip malformed event, keep reading
			}
			switch evt.Type {
			case "content_block_delta":
				if evt.Delta.Text != "" {
					select {
					case events <- StreamEvent{Delta: evt.Delta.Text}:
					case <-ctx.Done():
						return false
					}
				}
			case "message_start":
				inputTokens = evt.Message.Usage.InputTokens
			case "message_delta":
				if evt.Usage.OutputTokens > 0 {
					outputTokens = evt.Usage.OutputTokens
				}
			case "message_stop":
				return false
			}
			return true
		})
		if err != nil {
			events <- StreamEvent{Err: fmt.Errorf("%s: stream read: %w", p.id, err)}
			return
		}
		events <- StreamEvent{Done: true, Usage: &openai.Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		}}
	}()

	return events, nil
}
