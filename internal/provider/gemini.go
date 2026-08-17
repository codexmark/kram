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

const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"

// Gemini talks to Google's native streamGenerateContent endpoint (API key
// as a query param, "contents"/"parts" request shape, "user"/"model" roles
// instead of "user"/"assistant").
type Gemini struct {
	id      string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewGemini constructs the Gemini adapter. baseURL defaults to the public
// API when empty.
func NewGemini(id, baseURL, apiKey, model string) *Gemini {
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	return &Gemini{
		id:      id,
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *Gemini) ID() string   { return p.id }
func (p *Gemini) Kind() string { return "gemini" }

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
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

	var system *geminiContent
	contents := make([]geminiContent, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" {
			sc := geminiContent{Parts: []geminiPart{{Text: m.Content}}}
			system = &sc
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, geminiContent{Role: role, Parts: []geminiPart{{Text: m.Content}}})
	}

	body := geminiRequest{SystemInstruction: system, Contents: contents}
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
		return nil, fmt.Errorf("%s: upstream returned %s", p.id, resp.Status)
	}

	events := make(chan StreamEvent, 16)
	go func() {
		defer close(events)
		defer resp.Body.Close()

		var usage *openai.Usage
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
					if part.Text == "" {
						continue
					}
					select {
					case events <- StreamEvent{Delta: part.Text}:
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
