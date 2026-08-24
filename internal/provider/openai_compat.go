package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

// parseRetryAfter reads the seconds form of a Retry-After header value
// (e.g. "30") into a duration — the HTTP-date form ("Wed, 21 Oct 2015
// 07:28:00 GMT") is rare enough from the OpenAI-compatible providers
// Kram actually talks to that it's deliberately not handled here; an
// empty or unparseable value returns zero, and callers already treat
// zero as "no hint, use a computed backoff instead."
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	seconds, err := strconv.Atoi(header)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// OpenAICompatible talks to any backend that already speaks the OpenAI
// chat-completions wire format: OpenAI itself, OpenRouter, opencode zen,
// and most other aggregators. Only base URL, API key and an optional
// pinned model differ between them.
type OpenAICompatible struct {
	capabilities
	id      string
	baseURL string
	apiKey  string
	model   string // optional: overrides req.Model when set
	client  *http.Client
}

// NewOpenAICompatible constructs an adapter for an OpenAI-shaped backend.
func NewOpenAICompatible(id, baseURL, apiKey, model string, caps capabilities) *OpenAICompatible {
	return &OpenAICompatible{
		capabilities: caps,
		id:           id,
		baseURL:      baseURL,
		apiKey:       apiKey,
		model:        model,
		client:       &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *OpenAICompatible) ID() string   { return p.id }
func (p *OpenAICompatible) Kind() string { return "openai-compat" }

type openaiCompatChunk struct {
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Reasoning carries a reasoning-capable model's chain-of-
			// thought fragments, sent ahead of (sometimes long ahead of)
			// any real answer content. Two field names exist in the wild
			// for the same thing and both are populated here: "reasoning"
			// is OpenRouter's extension (gpt-oss, nemotron); "reasoning_content"
			// is what vLLM's reasoning parser and DeepSeek-R1-compatible
			// servers use instead — confirmed via a real streaming request
			// against a user's local server that was silently timing out
			// in router.BoundedPeek because this field went uncaptured.
			// See StreamEvent.Reasoning for why neither can just be folded
			// into Content.
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openai.Usage `json:"usage"`
}

// normalizeOpenAICompatMessages renders every system instruction as one
// leading message. OpenAI itself accepts multiple system messages, but a
// number of otherwise-compatible local chat templates (including Qwen 3.5
// in LM Studio) reject a second system message with "System message must be
// at the beginning". Kram deliberately compiles its base prompt, tool
// overview, project instructions, memory, and runtime reminders as separate
// system messages, so normalize at this adapter boundary instead of erasing
// those distinctions from the provider-independent prompt compiler.
func normalizeOpenAICompatMessages(messages []openai.ChatMessage) []openai.ChatMessage {
	firstSystem := -1
	systemCount := 0
	for i, msg := range messages {
		if msg.Role == "system" {
			if firstSystem < 0 {
				firstSystem = i
			}
			systemCount++
		}
	}
	if systemCount == 0 || (systemCount == 1 && firstSystem == 0) {
		return messages
	}

	merged := messages[firstSystem]
	parts := make([]string, 0, systemCount)
	nonSystem := make([]openai.ChatMessage, 0, len(messages)-systemCount)
	for _, msg := range messages {
		if msg.Role == "system" {
			if msg.Content != "" {
				parts = append(parts, msg.Content)
			}
			continue
		}
		nonSystem = append(nonSystem, msg)
	}
	merged.Content = strings.Join(parts, "\n\n")

	out := make([]openai.ChatMessage, 0, len(nonSystem)+1)
	out = append(out, merged)
	out = append(out, nonSystem...)
	return out
}

func (p *OpenAICompatible) ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (<-chan StreamEvent, error) {
	model := req.Model
	if p.model != "" {
		model = p.model
	}
	body := req
	body.Model = model
	body.Stream = true
	body.Messages = normalizeOpenAICompatMessages(sanitizeToolHistory(body.Messages))

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", p.id, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", p.id, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Custom local/LAN servers commonly run with no auth at all (see
	// internal/customprovider) — every other caller here always has a
	// real key, so skipping the header only when empty changes nothing
	// for them, but avoids sending a malformed "Bearer " to a server
	// that would otherwise just ignore it.
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", p.id, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, &HTTPError{
			Provider: p.id, StatusCode: resp.StatusCode, Status: resp.Status,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	events := make(chan StreamEvent, 16)
	go func() {
		defer close(events)
		defer resp.Body.Close()

		var usage *openai.Usage
		var upstreamErr error
		toolCalls := newToolCallAccumulator()
		err := scanSSEData(resp.Body, func(data string) bool {
			if data == "[DONE]" {
				return false
			}
			var chunk openaiCompatChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return true // skip malformed chunk, keep reading
			}
			if chunk.Error != nil {
				message := strings.TrimSpace(chunk.Error.Message)
				if message == "" {
					message = "unknown upstream stream error"
				}
				upstreamErr = fmt.Errorf("%s: upstream stream error: %s", p.id, message)
				return false
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
				} else if reasoning := c.Delta.Reasoning + c.Delta.ReasoningContent; reasoning != "" {
					// No real answer content yet, but reasoning output is
					// still real progress — forward it so
					// router.BoundedPeek doesn't mistake a thinking model
					// for a stalled one (see StreamEvent.Reasoning). Only
					// one of the two fields is ever non-empty for a given
					// server, so concatenating is safe — it's really just
					// "whichever one this server uses".
					select {
					case events <- StreamEvent{Reasoning: reasoning}:
					case <-ctx.Done():
						return false
					}
				} else if len(c.Delta.ToolCalls) > 0 {
					// A chunk carrying only tool-call argument fragments —
					// real progress, but not itself content or reasoning; see
					// StreamEvent.ToolCallProgress.
					select {
					case events <- StreamEvent{ToolCallProgress: true}:
					case <-ctx.Done():
						return false
					}
				}
				for _, tc := range c.Delta.ToolCalls {
					toolCalls.add(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
				}
			}
			return true
		})
		if err != nil {
			events <- StreamEvent{Err: fmt.Errorf("%s: stream read: %w", p.id, err)}
			return
		}
		if upstreamErr != nil {
			events <- StreamEvent{Err: upstreamErr}
			return
		}
		events <- StreamEvent{Done: true, Usage: usage, ToolCalls: toolCalls.finish()}
	}()

	return events, nil
}
