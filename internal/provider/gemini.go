package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codexmark/kram-gateway/internal/openai"
)

const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"

// Gemini talks to Google's native streamGenerateContent endpoint (API key
// as a query param, "contents"/"parts" request shape, "user"/"model" roles
// instead of "user"/"assistant", and tool calls as functionCall/
// functionResponse parts rather than a separate message role).
type Gemini struct {
	capabilities
	id      string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewGemini constructs the Gemini adapter. baseURL defaults to the public
// API when empty.
func NewGemini(id, baseURL, apiKey, model string, caps capabilities) *Gemini {
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	return &Gemini{
		capabilities: caps,
		id:           id,
		baseURL:      baseURL,
		apiKey:       apiKey,
		model:        model,
		client:       &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *Gemini) ID() string   { return p.id }
func (p *Gemini) Kind() string { return "gemini" }

type geminiPart struct {
	Text         string              `json:"text,omitempty"`
	InlineData   *geminiInlineData   `json:"inlineData,omitempty"`
	FunctionCall *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResp *geminiFunctionResp `json:"functionResponse,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	Tools             []geminiTool    `json:"tools,omitempty"`
}

// buildGeminiContents translates Kram's normalized messages into Gemini's
// role/parts shape. Assistant tool calls become functionCall parts; tool
// results (Kram's role:"tool") become a "function" role message carrying a
// functionResponse part.
func buildGeminiContents(msgs []openai.ChatMessage) (system *geminiContent, out []geminiContent) {
	for _, m := range msgs {
		switch m.Role {
		case "system":
			// Gemini accepts only one systemInstruction — concatenate
			// rather than let a later system message (e.g. a compaction
			// summary) silently clobber an earlier one (e.g. project
			// context from AGENTS.md).
			if system == nil {
				system = &geminiContent{Parts: []geminiPart{{Text: m.Content}}}
			} else {
				system.Parts[0].Text += "\n\n---\n\n" + m.Content
			}

		case "tool":
			var response map[string]any
			if err := json.Unmarshal([]byte(m.Content), &response); err != nil {
				response = map[string]any{"result": m.Content}
			}
			out = append(out, geminiContent{
				Role:  "function",
				Parts: []geminiPart{{FunctionResp: &geminiFunctionResp{Name: m.Name, Response: response}}},
			})

		case "assistant":
			var parts []geminiPart
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Function.Arguments)
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: tc.Function.Name, Args: args}})
			}
			out = append(out, geminiContent{Role: "model", Parts: parts})

		default: // user
			parts := []geminiPart{{Text: m.Content}}
			for _, img := range m.Images {
				if mime, data, ok := decodeDataURL(img); ok {
					parts = append(parts, geminiPart{InlineData: &geminiInlineData{MimeType: mime, Data: data}})
				}
			}
			out = append(out, geminiContent{Role: "user", Parts: parts})
		}
	}
	return system, out
}

// decodeDataURL splits a "data:<media-type>;base64,<data>" URL into its
// mime type and base64 payload.
func decodeDataURL(url string) (mimeType, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}
	rest := url[len(prefix):]
	semi := strings.Index(rest, ";")
	comma := strings.Index(rest, ",")
	if semi < 0 || comma < 0 || comma < semi {
		return "", "", false
	}
	return rest[:semi], rest[comma+1:], true
}

func buildGeminiTools(tools []openai.Tool) []geminiTool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]geminiFunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, geminiFunctionDeclaration{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	return []geminiTool{{FunctionDeclarations: decls}}
}

type geminiStreamChunk struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func (p *Gemini) ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (<-chan StreamEvent, error) {
	model := req.Model
	if p.model != "" {
		model = p.model
	}

	system, contents := buildGeminiContents(req.Messages)

	body := geminiRequest{SystemInstruction: system, Contents: contents, Tools: buildGeminiTools(req.Tools)}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", p.id, err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s", p.baseURL, model, p.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", p.id, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
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

		var usage *openai.Usage
		var toolCalls []openai.ToolCall
		callIndex := 0
		err := scanSSEData(resp.Body, func(data string) bool {
			var chunk geminiStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return true // skip malformed chunk, keep reading
			}
			if chunk.UsageMetadata.TotalTokenCount > 0 {
				usage = &openai.Usage{
					PromptTokens:     chunk.UsageMetadata.PromptTokenCount,
					CompletionTokens: chunk.UsageMetadata.CandidatesTokenCount,
					TotalTokens:      chunk.UsageMetadata.TotalTokenCount,
				}
			}
			for _, c := range chunk.Candidates {
				for _, part := range c.Content.Parts {
					if part.Text != "" {
						select {
						case events <- StreamEvent{Delta: part.Text}:
						case <-ctx.Done():
							return false
						}
					}
					if part.FunctionCall != nil {
						callIndex++
						args := part.FunctionCall.Args
						if len(args) == 0 {
							args = json.RawMessage("{}")
						}
						toolCalls = append(toolCalls, openai.ToolCall{
							ID:   fmt.Sprintf("call_%d", callIndex),
							Type: "function",
							Function: openai.ToolCallFunction{
								Name:      part.FunctionCall.Name,
								Arguments: string(args),
							},
						})
					}
				}
			}
			return true
		})
		if err != nil {
			events <- StreamEvent{Err: fmt.Errorf("%s: stream read: %w", p.id, err)}
			return
		}
		events <- StreamEvent{Done: true, Usage: usage, ToolCalls: toolCalls}
	}()

	return events, nil
}
