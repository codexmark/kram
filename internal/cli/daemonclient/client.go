// Package daemonclient is the CLI's HTTP client for a running kram-daemon:
// creating sessions and sending messages. The CLI itself never persists
// anything or talks to an LLM provider — it's purely a view over what the
// daemon already owns.
package daemonclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	stream  *http.Client // no fixed timeout: a multi-tool agent turn can legitimately run long; the caller's context is the only bound
}

// New builds a client pointed at a running daemon (e.g. http://127.0.0.1:20130).
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
		stream:  &http.Client{},
	}
}

// CreateSession starts a new session with the given title.
func (c *Client) CreateSession(ctx context.Context, title string) (Session, error) {
	var sess Session
	err := c.doJSON(ctx, http.MethodPost, "/sessions", map[string]string{"title": title}, &sess)
	return sess, err
}

// ListSessions returns every known session, most recently active first.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	var out []Session
	err := c.doJSON(ctx, http.MethodGet, "/sessions", nil, &out)
	return out, err
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
// producing this reply. Running is CLI-only UI state (never sent over the
// wire) — true from the moment a tool_start event arrives until its
// matching tool_result lands.
type ToolActivity struct {
	Name    string `json:"name"`
	Args    string `json:"args"`
	Result  string `json:"result"`
	OK      bool   `json:"ok"`
	Running bool   `json:"-"`
}

// StreamEvent is one event from a message stream. Type selects which
// other fields are meaningful: "delta" (Content), "tool_start" (Name,
// Args), "tool_result" (Name, Result, OK), "notice" (Text), "question"
// (QuestionID, Question, Options), "done" (Message, Attempts, Usage,
// ToolActivity, Compactions, ImageNotice), or "error" (Error).
type StreamEvent struct {
	Type         string               `json:"type"`
	Content      string               `json:"content,omitempty"`
	Name         string               `json:"name,omitempty"`
	Args         string               `json:"args,omitempty"`
	Result       string               `json:"result,omitempty"`
	OK           bool                 `json:"ok,omitempty"`
	Text         string               `json:"text,omitempty"`
	QuestionID   string               `json:"question_id,omitempty"`
	Question     string               `json:"question,omitempty"`
	Options      []string             `json:"options,omitempty"`
	Message      Message              `json:"message,omitempty"`
	Attempts     []openai.AttemptInfo `json:"attempts,omitempty"`
	Usage        openai.Usage         `json:"usage,omitempty"`
	ToolActivity []ToolActivity       `json:"tool_activity,omitempty"`
	Compactions  int                  `json:"compactions,omitempty"`
	ImageNotice  string               `json:"image_notice,omitempty"`
	Error        string               `json:"error,omitempty"`
}

// MessageStream is an open connection to a running agent turn — read it
// with Next until it reports done, then Close it.
type MessageStream struct {
	resp    *http.Response
	scanner *bufio.Scanner
}

// Next blocks until the next SSE event arrives. done is true once the
// stream has logically ended (a "done"/"error" event, EOF, or a read
// error) — the caller should stop calling Next at that point, though it
// remains safe to call again (it will just report done with a zero event).
func (s *MessageStream) Next() (evt StreamEvent, done bool, err error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return StreamEvent{}, true, nil
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue // skip malformed event, keep reading
		}
		return evt, evt.Type == "done" || evt.Type == "error", nil
	}
	if err := s.scanner.Err(); err != nil {
		return StreamEvent{}, true, fmt.Errorf("reading daemon stream: %w", err)
	}
	return StreamEvent{}, true, nil // EOF
}

// Close releases the underlying HTTP connection.
func (s *MessageStream) Close() error {
	return s.resp.Body.Close()
}

// SendMessageStream posts a user message (with optional image data: URLs)
// and returns a live stream of what the agent loop does to answer it —
// text deltas as they're generated, tool activity, notices, ending in one
// "done" (or "error") event.
func (c *Client) SendMessageStream(ctx context.Context, sessionID, content string, images []string) (*MessageStream, error) {
	body := map[string]any{"content": content}
	if len(images) > 0 {
		body["images"] = images
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions/"+sessionID+"/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling daemon: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var errBody struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&errBody) == nil && errBody.Error != "" {
			return nil, fmt.Errorf("daemon: %s", errBody.Error)
		}
		return nil, fmt.Errorf("daemon returned %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &MessageStream{resp: resp, scanner: scanner}, nil
}

// AnswerQuestion delivers an answer to a pending ask_question call — the
// SSE stream that produced the "question" event stays open and blocked
// server-side the whole time; this is a separate HTTP request that
// unblocks it, not a message sent over the stream itself.
func (c *Client) AnswerQuestion(ctx context.Context, sessionID, questionID, answer string) error {
	body := map[string]string{"question_id": questionID, "answer": answer}
	return c.doJSON(ctx, http.MethodPost, "/sessions/"+sessionID+"/answer", body, nil)
}

// ToolInfo mirrors the daemon's tools.ToolInfo — one registered tool,
// enabled or not, for the tools/skills toggle screen.
type ToolInfo struct {
	Name        string `json:"Name"`
	Description string `json:"Description"`
	Disabled    bool   `json:"Disabled"`
}

// Skill mirrors the daemon's tools.Skill.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
	Disabled    bool   `json:"disabled"`
}

// ListTools fetches every registered tool and discovered skill from the
// daemon, enabled or not — the source of truth the toggle screen renders
// from, rather than a hardcoded duplicate list in the CLI.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, []Skill, error) {
	var out struct {
		Tools  []ToolInfo `json:"tools"`
		Skills []Skill    `json:"skills"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/tools", nil, &out)
	return out.Tools, out.Skills, err
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
