// Package daemonclient is the CLI's HTTP client for a running kram-daemon:
// creating sessions and sending messages. The CLI itself never persists
// anything or talks to an LLM provider — it's purely a view over what the
// daemon already owns.
package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/codexmark/kram-gateway/internal/openai"
)

// Message mirrors the daemon's persisted message shape.
type Message struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Provider  string `json:"provider,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// Session mirrors the daemon's persisted session shape.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Client talks to a kram-daemon instance.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a client pointed at a running daemon (e.g. http://127.0.0.1:20130).
func New(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 180 * time.Second}}
}

// CreateSession starts a new session with the given title.
func (c *Client) CreateSession(ctx context.Context, title string) (Session, error) {
	var sess Session
	err := c.doJSON(ctx, http.MethodPost, "/sessions", map[string]string{"title": title}, &sess)
	return sess, err
}

// GetSession returns a session and its full message history.
func (c *Client) GetSession(ctx context.Context, id string) (Session, []Message, error) {
	var out struct {
		Session  Session   `json:"session"`
		Messages []Message `json:"messages"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/sessions/"+id, nil, &out)
	return out.Session, out.Messages, err
}

// ToolActivity mirrors one tool call the daemon's agent loop made while
// producing this reply.
type ToolActivity struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Result string `json:"result"`
	OK     bool   `json:"ok"`
}

// SendMessageResult is a reply plus the real fallback trail the gateway
// walked to produce it, and everything the agent loop did along the way.
type SendMessageResult struct {
	Message      Message              `json:"message"`
	Attempts     []openai.AttemptInfo `json:"attempts"`
	Usage        openai.Usage         `json:"usage"`
	ToolActivity []ToolActivity       `json:"tool_activity"`
	Compactions  int                  `json:"compactions"`
	ImageNotice  string               `json:"image_notice"`
}

// SendMessage posts a user message (with optional image data: URLs) to a
// session and returns the assistant's reply.
func (c *Client) SendMessage(ctx context.Context, sessionID, content string, images []string) (SendMessageResult, error) {
	var out SendMessageResult
	body := map[string]any{"content": content}
	if len(images) > 0 {
		body["images"] = images
	}
	err := c.doJSON(ctx, http.MethodPost, "/sessions/"+sessionID+"/messages", body, &out)
	return out, err
}

// ContextCategory is one real contributor to a session's context-window usage.
type ContextCategory struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// ContextUsage is a session's current context-window breakdown.
type ContextUsage struct {
	Budget     int               `json:"budget"`
	Used       int               `json:"used"`
	Free       int               `json:"free"`
	Categories []ContextCategory `json:"categories"`
}

// GetContext fetches a session's current context-window usage breakdown.
func (c *Client) GetContext(ctx context.Context, sessionID string) (ContextUsage, error) {
	var out ContextUsage
	err := c.doJSON(ctx, http.MethodGet, "/sessions/"+sessionID+"/context", nil, &out)
	return out, err
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling daemon: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errBody struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&errBody) == nil && errBody.Error != "" {
			return fmt.Errorf("daemon: %s", errBody.Error)
		}
		return fmt.Errorf("daemon returned %s", resp.Status)
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding daemon response: %w", err)
	}
	return nil
}
