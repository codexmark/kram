package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

const defaultAnthropicBaseURL = "https://api.anthropic.com"

// Anthropic talks to the native Messages API (x-api-key auth, "system" is a
// top-level field rather than a message role, streaming uses named SSE
// events instead of OpenAI's flat "data:" chunks, and tool use/results are
// content blocks rather than separate message roles).
//
// apiKey is always a real, permanent Anthropic API key — including when
// the account was connected via the wizard's browser-login flow: that
// flow (internal/oauthflow.AnthropicAuthorize) exchanges the OAuth token
// for one of these right away rather than handing back something
// short-lived, since a raw Claude Pro/Max OAuth token turned out not to
// be a usable inference credential on its own (see that file's doc
// comment for what was actually live-verified). So this adapter never
// needed a second, refreshable credential shape the way
// internal/provider/openai_responses.go does.
type Anthropic struct {
	capabilities
	id      string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewAnthropic constructs the Anthropic adapter. baseURL defaults to the
// public API when empty.
func NewAnthropic(id, baseURL, apiKey, model string, caps capabilities) *Anthropic {
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}
	return &Anthropic{
		capabilities: caps,
		id:           id,
		baseURL:      baseURL,
		apiKey:       apiKey,
		model:        model,
		client:       &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *Anthropic) ID() string   { return p.id }
func (p *Anthropic) Kind() string { return "anthropic" }

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// tool_use (assistant → provider is asking to call a tool)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (kram → provider is reporting what a tool returned)
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

// buildMessages translates Kram's normalized messages into Anthropic's
// content-block form. Tool results become a "user" message carrying a
// tool_result block (Anthropic has no separate "tool" role); assistant
// tool calls become tool_use blocks alongside any text.
func buildAnthropicMessages(msgs []openai.ChatMessage) (system string, out []anthropicMessage) {
	for _, m := range msgs {
		switch m.Role {
		case "system":
			// Anthropic accepts only one top-level "system" field —
			// concatenate rather than let a later system message (e.g. a
			// compaction summary) silently clobber an earlier one (e.g.
			// project context from AGENTS.md).
			if system != "" {
				system += "\n\n---\n\n"
			}
			system += m.Content

		case "tool":
			out = append(out, anthropicMessage{
				Role:    "user",
				Content: []anthropicContentBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}},
			})

		case "assistant":
			var blocks []anthropicContentBlock
			if m.Content != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropicContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})

		default: // user
			blocks := []anthropicContentBlock{{Type: "text", Text: m.Content}}
			for _, img := range m.Images {
				if src := parseDataURL(img); src != nil {
					blocks = append(blocks, anthropicContentBlock{Type: "image", Source: src})
				}
			}
			out = append(out, anthropicMessage{Role: m.Role, Content: blocks})
		}
	}
	return system, out
}

// parseDataURL splits a "data:<media-type>;base64,<data>" URL into an
// Anthropic image source, or returns nil if it isn't one.
func parseDataURL(url string) *anthropicImageSource {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil
	}
	rest := url[len(prefix):]
	semi := strings.Index(rest, ";")
	comma := strings.Index(rest, ",")
	if semi < 0 || comma < 0 || comma < semi {
		return nil
	}
	mediaType := rest[:semi]
	data := rest[comma+1:]
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil
	}
	return &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}
}

func buildAnthropicTools(tools []openai.Tool) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropicTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: t.Function.Parameters})
	}
	return out
}

type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
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

	system, messages := buildAnthropicMessages(req.Messages)

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
		Tools:     buildAnthropicTools(req.Tools),
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
		return nil, &HTTPError{Provider: p.id, StatusCode: resp.StatusCode, Status: resp.Status}
	}

	events := make(chan StreamEvent, 16)
	go func() {
		defer close(events)
		defer resp.Body.Close()

		var inputTokens, outputTokens int
		// Anthropic streams tool_use blocks as a content_block_start
		// (carrying id/name) followed by input_json_delta fragments — track
		// which block index is a tool_use and accumulate its JSON.
		blockIsToolUse := map[int]*openai.ToolCall{}
		var toolCalls []openai.ToolCall

		err := scanSSEData(resp.Body, func(data string) bool {
			var evt anthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				return true // skip malformed event, keep reading
			}
			switch evt.Type {
			case "content_block_start":
				if evt.ContentBlock.Type == "tool_use" {
					blockIsToolUse[evt.Index] = &openai.ToolCall{
						ID:   evt.ContentBlock.ID,
						Type: "function",
						Function: openai.ToolCallFunction{
							Name: evt.ContentBlock.Name,
						},
					}
				}
			case "content_block_delta":
				if tc, ok := blockIsToolUse[evt.Index]; ok {
					tc.Function.Arguments += evt.Delta.PartialJSON
					// Real progress, but not itself content or reasoning —
					// see StreamEvent.ToolCallProgress.
					select {
					case events <- StreamEvent{ToolCallProgress: true}:
					case <-ctx.Done():
						return false
					}
				} else if evt.Delta.Text != "" {
					select {
					case events <- StreamEvent{Delta: evt.Delta.Text}:
					case <-ctx.Done():
						return false
					}
				}
			case "content_block_stop":
				if tc, ok := blockIsToolUse[evt.Index]; ok {
					toolCalls = append(toolCalls, *tc)
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
		events <- StreamEvent{Done: true, ToolCalls: toolCalls, Usage: &openai.Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		}}
	}()

	return events, nil
}
