package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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
		client:       &http.Client{Timeout: DefaultTimeout},
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

	// encrypted reasoning replay (store:false continuity)
	ID               string          `json:"id,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"`
}

type responsesTool struct {
	Type         string          `json:"type"` // "function" or "tool_search"
	Name         string          `json:"name,omitempty"`
	Description  string          `json:"description,omitempty"`
	Parameters   json.RawMessage `json:"parameters,omitempty"`
	DeferLoading bool            `json:"defer_loading,omitempty"`
}

type responsesRequest struct {
	Model        string               `json:"model"`
	Instructions string               `json:"instructions,omitempty"`
	Input        []responsesInputItem `json:"input"`
	Stream       bool                 `json:"stream"`
	// The ChatGPT-authenticated Codex backend rejects requests unless
	// persistence is disabled explicitly ("Store must be set to false").
	// Keep the field non-omitempty so false is present on the wire.
	Store          bool            `json:"store"`
	Tools          []responsesTool `json:"tools,omitempty"`
	Include        []string        `json:"include,omitempty"`
	PromptCacheKey string          `json:"prompt_cache_key,omitempty"`
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
			for _, item := range m.ProviderItems {
				if item.Type == "reasoning" && item.EncryptedContent != "" {
					out = append(out, responsesInputItem{Type: item.Type, ID: item.ID, EncryptedContent: item.EncryptedContent, Summary: item.Summary})
				}
			}
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

func buildResponsesTools(tools []openai.Tool, deferred bool) []responsesTool {
	if len(tools) == 0 {
		return nil
	}
	extra := 0
	if deferred {
		extra = 1
	}
	out := make([]responsesTool, 0, len(tools)+extra)
	for _, t := range tools {
		out = append(out, responsesTool{Type: "function", Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters, DeferLoading: deferred})
	}
	// GPT-5.4+ can discover deferred function schemas only when needed. The
	// complete inventory remains known to hosted tool search, but schemas no
	// longer consume every tool-calling round's prompt.
	if deferred {
		out = append(out, responsesTool{Type: "tool_search"})
	}
	return out
}

func supportsHostedToolSearch(model string) bool {
	return strings.HasPrefix(model, "gpt-5.4") || strings.HasPrefix(model, "gpt-5.5") || strings.HasPrefix(model, "gpt-5.6")
}

// responsesStreamEvent covers the handful of Responses API streaming
// event types this adapter needs: text deltas, function-call argument
// deltas, a new output item starting (to learn a function call's id/name
// before its argument deltas arrive), and the terminal "completed" event
// carrying usage.
type responsesStreamEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	Arguments   string `json:"arguments"`
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Item        struct {
		ID               string          `json:"id"`
		Type             string          `json:"type"`
		CallID           string          `json:"call_id"`
		Name             string          `json:"name"`
		Arguments        string          `json:"arguments"`
		EncryptedContent string          `json:"encrypted_content"`
		Summary          json.RawMessage `json:"summary"`
	} `json:"item"`
	Response struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func (p *OpenAIResponses) ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (<-chan StreamEvent, error) {
	model := req.Model
	if p.model != "" {
		model = p.model
	}

	instructions, input := buildResponsesInput(sanitizeToolHistory(req.Messages))

	body := responsesRequest{
		Model:          model,
		Instructions:   instructions,
		Input:          input,
		Stream:         true,
		Store:          false,
		Tools:          buildResponsesTools(req.Tools, supportsHostedToolSearch(model)),
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: req.PromptCacheKey,
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
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &HTTPError{
			Provider: p.id, StatusCode: resp.StatusCode, Status: resp.Status,
			Detail: strings.Join(strings.Fields(string(detail)), " "),
		}
	}

	events := make(chan StreamEvent, 16)
	go func() {
		defer close(events)
		defer resp.Body.Close()

		toolCalls := newToolCallAccumulator()
		toolArgsSeen := map[int]bool{}
		var inputTokens, outputTokens, cachedTokens, cacheWriteTokens, reasoningTokens int
		var providerItems []openai.ProviderItem

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
					toolCalls.add(evt.OutputIndex, evt.Item.CallID, evt.Item.Name, evt.Item.Arguments)
					toolArgsSeen[evt.OutputIndex] = evt.Item.Arguments != ""
				}
			case "response.output_item.done":
				if evt.Item.Type == "reasoning" && evt.Item.EncryptedContent != "" {
					providerItems = append(providerItems, openai.ProviderItem{Type: "reasoning", ID: evt.Item.ID, EncryptedContent: evt.Item.EncryptedContent, Summary: evt.Item.Summary})
				}
			case "response.function_call_arguments.delta":
				// Codex identifies argument deltas by item_id/output_index,
				// not by nesting call_id under item. OutputIndex is therefore
				// the stable join key; using an absent call_id here used to
				// create a second, nameless phantom tool call.
				toolCalls.add(evt.OutputIndex, "", "", evt.Delta)
				toolArgsSeen[evt.OutputIndex] = true
			case "response.function_call_arguments.done":
				// Some server builds send the complete arguments only in the
				// done event. Do not append them when deltas already assembled
				// the same value.
				if !toolArgsSeen[evt.OutputIndex] && evt.Arguments != "" {
					toolCalls.add(evt.OutputIndex, "", "", evt.Arguments)
				}
			case "response.completed":
				inputTokens = evt.Response.Usage.InputTokens
				outputTokens = evt.Response.Usage.OutputTokens
				cachedTokens = evt.Response.Usage.InputTokensDetails.CachedTokens
				cacheWriteTokens = evt.Response.Usage.InputTokensDetails.CacheWriteTokens
				reasoningTokens = evt.Response.Usage.OutputTokensDetails.ReasoningTokens
				return false
			}
			return true
		})
		if err != nil {
			events <- StreamEvent{Err: fmt.Errorf("%s: stream read: %w", p.id, err)}
			return
		}
		usage := &openai.Usage{
			PromptTokens:       inputTokens,
			CompletionTokens:   outputTokens,
			TotalTokens:        inputTokens + outputTokens,
			CachedPromptTokens: cachedTokens, CacheWritePromptTokens: cacheWriteTokens,
			ReasoningTokens: reasoningTokens,
		}
		usage.EstimatedCostMicros = responsesEquivalentCostMicros(model, *usage)
		events <- StreamEvent{Done: true, ToolCalls: toolCalls.finish(), ProviderItems: providerItems, Usage: usage}
	}()

	return events, nil
}

func responsesEquivalentCostMicros(model string, usage openai.Usage) int64 {
	if !strings.HasPrefix(model, "gpt-5.5") && !strings.HasPrefix(model, "gpt-5.6") {
		return 0
	}
	uncached := usage.PromptTokens - usage.CachedPromptTokens - usage.CacheWritePromptTokens
	if uncached < 0 {
		uncached = 0
	}
	// Current GPT-5.5/5.6 list price: $5/M input, $0.50/M cache read,
	// $30/M output. GPT-5.6 cache writes are $6.25/M; 5.5 uses input rate.
	writeRate := int64(5_000_000)
	if strings.HasPrefix(model, "gpt-5.6") {
		writeRate = 6_250_000
	}
	return (int64(uncached)*5_000_000 + int64(usage.CachedPromptTokens)*500_000 + int64(usage.CacheWritePromptTokens)*writeRate + int64(usage.CompletionTokens)*30_000_000) / 1_000_000
}
