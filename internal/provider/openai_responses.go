package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

const defaultOpenAIResponsesBaseURL = "https://chatgpt.com/backend-api/codex/responses"

// OpenAIResponses talks to the Codex backend a ChatGPT Plus/Pro/Team
// subscription unlocks via browser login (internal/oauthflow.OpenAIAuthorize)
// — a different product from the standard OpenAI developer API that
// OpenAICompatible (openai_compat.go) talks to. It uses OpenAI's Responses
// wire format ("input" items instead of "messages", typed streaming
// events instead of flat delta chunks) and only serves a restricted set
// of Codex-branded models, never the general OpenAI catalog.
//
// This adapter is experimental: its request/response shape follows
// OpenAI's publicly documented Responses API, but the exact headers this
// specific ChatGPT-authenticated backend expects beyond
// "Authorization: Bearer" were not fully confirmed against a real account
// before shipping (see DECISIONS.md) — treat failures here as a signal to
// re-check against a live subscription, not necessarily a Kram bug.
type OpenAIResponses struct {
	capabilities
	id      string
	baseURL string
	model   string
	resolve func(context.Context) (string, error)
	client  *http.Client
}

// NewOpenAIResponses constructs the adapter. resolve is always required —
// unlike every other adapter, there is no static-API-key path for this
// product; a credential only ever comes from a refreshable OAuth token
// (see internal/credentials.Store.Resolve).
func NewOpenAIResponses(id, baseURL string, resolve func(context.Context) (string, error), model string, caps capabilities) *OpenAIResponses {
	if baseURL == "" {
		baseURL = defaultOpenAIResponsesBaseURL
	}
	return &OpenAIResponses{
		capabilities: caps,
		id:           id,
		baseURL:      baseURL,
		model:        model,
		resolve:      resolve,
		client:       &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *OpenAIResponses) ID() string   { return p.id }
func (p *OpenAIResponses) Kind() string { return "openai-responses" }

type responsesInputContent struct {
	Type string `json:"type"` // "input_text" or "output_text"
	Text string `json:"text"`
}

type responsesInputItem struct {
	Type string `json:"type"` // "message", "function_call", or "function_call_output"

	// message
	Role    string                  `json:"role,omitempty"`
	Content []responsesInputContent `json:"content,omitempty"`

	// function_call (assistant → provider requested a tool call)
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output (kram → reporting what a tool returned)
	Output string `json:"output,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type responsesRequest struct {
	Model        string               `json:"model"`
	Instructions string               `json:"instructions,omitempty"`
	Input        []responsesInputItem `json:"input"`
	Stream       bool                 `json:"stream"`
	Tools        []responsesTool      `json:"tools,omitempty"`
}

// buildResponsesInput translates Kram's normalized messages into the
// Responses API's flat "input" item list — mirrors buildAnthropicMessages
// (anthropic.go) in spirit: system prompts are pulled out to a top-level
// field, tool calls/results become their own item types rather than
// message roles.
func buildResponsesInput(msgs []openai.ChatMessage) (instructions string, out []responsesInputItem) {
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if instructions != "" {
				instructions += "\n\n---\n\n"
			}
			instructions += m.Content

		case "tool":
			out = append(out, responsesInputItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})

		case "assistant":
			if m.Content != "" {
				out = append(out, responsesInputItem{
					Type: "message", Role: "assistant",
					Content: []responsesInputContent{{Type: "output_text", Text: m.Content}},
				})
			}
			for _, tc := range m.ToolCalls {
				out = append(out, responsesInputItem{
					Type: "function_call", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
				})
			}

		default: // user
			out = append(out, responsesInputItem{
				Type: "message", Role: m.Role,
				Content: []responsesInputContent{{Type: "input_text", Text: m.Content}},
			})
		}
	}
	return instructions, out
}

func buildResponsesTools(tools []openai.Tool) []responsesTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]responsesTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, responsesTool{Type: "function", Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	return out
}

// responsesStreamEvent covers the handful of Responses API streaming
// event types this adapter needs: text deltas, function-call argument
// deltas, a new output item starting (to learn a function call's id/name
// before its argument deltas arrive), and the terminal "completed" event
// carrying usage.
type responsesStreamEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Item  struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"response"`
}

func (p *OpenAIResponses) ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (<-chan StreamEvent, error) {
	model := req.Model
	if p.model != "" {
		model = p.model
	}

	instructions, input := buildResponsesInput(req.Messages)

	body := responsesRequest{
		Model:        model,
		Instructions: instructions,
		Input:        input,
		Stream:       true,
		Tools:        buildResponsesTools(req.Tools),
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", p.id, err)
	}

	token, err := p.resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: resolving credential: %w", p.id, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", p.id, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
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

		toolCalls := newToolCallAccumulator()
		toolCallIndex := map[string]int{} // call_id -> accumulator index
		var inputTokens, outputTokens int

		err := scanSSEData(resp.Body, func(data string) bool {
			var evt responsesStreamEvent
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				return true // skip malformed event, keep reading
			}
			switch evt.Type {
			case "response.output_text.delta":
				if evt.Delta != "" {
					select {
					case events <- StreamEvent{Delta: evt.Delta}:
					case <-ctx.Done():
						return false
					}
				}
			case "response.output_item.added":
				if evt.Item.Type == "function_call" {
					idx := len(toolCallIndex)
					toolCallIndex[evt.Item.CallID] = idx
					toolCalls.add(idx, evt.Item.CallID, evt.Item.Name, evt.Item.Arguments)
				}
			case "response.function_call_arguments.delta":
				// The delta carries the call_id via Item.CallID on some
				// server builds and only positionally otherwise; when
				// unknown, accumulate under a synthetic trailing index
				// rather than dropping the fragment.
				idx, ok := toolCallIndex[evt.Item.CallID]
				if !ok {
					idx = len(toolCallIndex)
					toolCallIndex[evt.Item.CallID] = idx
				}
				toolCalls.add(idx, "", "", evt.Delta)
			case "response.completed":
				inputTokens = evt.Response.Usage.InputTokens
				outputTokens = evt.Response.Usage.OutputTokens
				return false
			}
			return true
		})
		if err != nil {
			events <- StreamEvent{Err: fmt.Errorf("%s: stream read: %w", p.id, err)}
			return
		}
		events <- StreamEvent{Done: true, ToolCalls: toolCalls.finish(), Usage: &openai.Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		}}
	}()

	return events, nil
}
