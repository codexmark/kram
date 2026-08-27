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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/openai"
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
	baseURL   string
	authToken string // sent as "Authorization: Bearer <token>" on every request — see daemon.Config.AuthToken
	http      *http.Client
	stream    *http.Client // no fixed timeout: a multi-tool agent turn can legitimately run long; the caller's context is the only bound
}

// New builds a client pointed at a running daemon (e.g.
// http://127.0.0.1:20130). authToken is the per-process bearer token the
// daemon requires on every request (may be "" when talking to a daemon
// started without auth, e.g. an old build — the header is simply omitted
// then). The single-binary cmd/kram threads the same token it hands the
// daemon; a standalone CLI passes the token the daemon wrote to its
// daemon.token file.
func New(baseURL, authToken string) *Client {
	return &Client{
		baseURL:   baseURL,
		authToken: authToken,
		http:      &http.Client{Timeout: 30 * time.Second},
		stream:    &http.Client{},
	}
}

// authorize adds the bearer header when a token is configured — the one
// place both request-building sites (doJSON and SendMessageStream) route
// through, so neither can forget it.
func (c *Client) authorize(req *http.Request) {
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
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
	Name      string `json:"name"`
	Args      string `json:"args"`
	Result    string `json:"result"`
	OK        bool   `json:"ok"`
	ProcessID string `json:"process_id,omitempty"`
	Running   bool   `json:"-"`
}

// RouteCall mirrors agent.RouteCall — one model call's full routing
// story within a run.
type RouteCall struct {
	Index    int                         `json:"index"`
	Combo    string                      `json:"combo"`
	Strategy string                      `json:"strategy"`
	Attempts []openai.AttemptInfo        `json:"attempts"`
	Ranking  []openai.RankedProviderInfo `json:"ranking,omitempty"`
	Winner   string                      `json:"winner,omitempty"`
}

// RouteTrace mirrors agent.RouteTrace — every model call a run made, in
// order, each with its own complete fallback trail. Distinct from
// StreamEvent.Attempts (the last call's trail only, kept for the simple
// footer view): RouteTrace is what a full route-trace UI (Ctrl+R) needs.
type RouteTrace struct {
	Combo    string      `json:"combo"`
	Strategy string      `json:"strategy"`
	Calls    []RouteCall `json:"calls"`
}

// StreamEvent is one event from a message stream. Type selects which
// other fields are meaningful: "delta" (Content), "reasoning" (Content —
// a reasoning-capable model's chain-of-thought fragment, best-effort and
// never the model's actual answer; see agent.EventReasoning), "tool_start"
// (Name, Args), "tool_result" (Name, Result, OK), "notice" (Text),
// "question" (QuestionID, Question, Options), "approval" (ApprovalID,
// Tool, Subject, Options), "route_start" (none), "route_done" (RouteCall),
// "heartbeat", "segment" (Segment/Segments), "done"
// (Message, Attempts, RouteTrace, Usage, ToolActivity, Compactions,
// ImageNotice), or "error" (Error).
type StreamEvent struct {
	Type         string               `json:"type"`
	Content      string               `json:"content,omitempty"`
	Name         string               `json:"name,omitempty"`
	Args         string               `json:"args,omitempty"`
	Result       string               `json:"result,omitempty"`
	OK           bool                 `json:"ok,omitempty"`
	ProcessID    string               `json:"process_id,omitempty"`
	Segment      int                  `json:"segment,omitempty"`
	Segments     int                  `json:"segments,omitempty"`
	Text         string               `json:"text,omitempty"`
	QuestionID   string               `json:"question_id,omitempty"`
	Question     string               `json:"question,omitempty"`
	Options      []string             `json:"options,omitempty"`
	ApprovalID   string               `json:"approval_id,omitempty"`
	Tool         string               `json:"tool,omitempty"`
	Subject      string               `json:"subject,omitempty"`
	Diff         string               `json:"diff,omitempty"` // approval: unified diff for edit_file/write_file
	RouteCall    *RouteCall           `json:"route_call,omitempty"`
	Message      Message              `json:"message,omitempty"`
	Attempts     []openai.AttemptInfo `json:"attempts,omitempty"`
	RouteTrace   RouteTrace           `json:"route_trace,omitempty"`
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

// Close releases the underlying HTTP connection. Safe to call on a
// zero-value or already-closed stream (nil resp) — the CLI's interrupt
// path closes whatever stream it holds without first proving it was ever
// opened.
func (s *MessageStream) Close() error {
	if s == nil || s.resp == nil {
		return nil
	}
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
	c.authorize(req)

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

// AnswerApproval delivers a decision ("once", "always", or "deny") to a
// pending permission-policy approval — same blocking shape as
// AnswerQuestion, but a distinct endpoint/id space (see server.go).
func (c *Client) AnswerApproval(ctx context.Context, sessionID, approvalID, decision string) error {
	body := map[string]string{"approval_id": approvalID, "decision": decision}
	return c.doJSON(ctx, http.MethodPost, "/sessions/"+sessionID+"/approve", body, nil)
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

// UpdateToolSettings applies the already-persisted disabled set to the live
// daemon. This keeps the settings UI and the registry used by the next model
// call coherent without requiring a process restart.
func (c *Client) UpdateToolSettings(ctx context.Context, disabled []string) error {
	return c.doJSON(ctx, http.MethodPut, "/tools/settings", map[string]any{"disabled": disabled}, nil)
}

// SetCombo switches the daemon's active combo — the combo ID future
// messages route to. Takes effect on the next message; an in-flight turn
// keeps the combo it started with.
func (c *Client) SetCombo(ctx context.Context, combo string) error {
	return c.doJSON(ctx, http.MethodPost, "/combo", map[string]string{"combo": combo}, nil)
}

// RewindCheckpoint mirrors the daemon's snapshot metadata for the
// automatic pre-mutation checkpoint a one-key rewind would restore to.
type RewindCheckpoint struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// ShortID mirrors snapshot.Snapshot.ShortID for display.
func (c RewindCheckpoint) ShortID() string {
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
}

// RewindResult is what a rewind actually changed.
type RewindResult struct {
	Restored struct {
		SnapshotID string `json:"snapshot_id"`
		Changes    []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"changes"`
	} `json:"restored"`
	Snapshot RewindCheckpoint `json:"snapshot"`
}

// RewindInfo fetches the newest automatic checkpoint — what Rewind would
// restore to — for a confirm-before-destroy flow.
func (c *Client) RewindInfo(ctx context.Context) (RewindCheckpoint, error) {
	var out RewindCheckpoint
	err := c.doJSON(ctx, http.MethodGet, "/rewind", nil, &out)
	return out, err
}

// Rewind restores the workspace to the given checkpoint id (from
// RewindInfo — passing the id pins the confirmation to exactly what the
// user was shown).
func (c *Client) Rewind(ctx context.Context, id string) (RewindResult, error) {
	var out RewindResult
	err := c.doJSON(ctx, http.MethodPost, "/rewind", map[string]string{"id": id}, &out)
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

// BackgroundProcess mirrors the daemon's read-only process metadata.
type BackgroundProcess struct {
	ID            string     `json:"id"`
	Command       string     `json:"command"`
	PID           int        `json:"pid"`
	Running       bool       `json:"running"`
	ExitCode      int        `json:"exit_code"`
	ExitError     string     `json:"exit_error,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	OutputBytes   int64      `json:"output_bytes"`
	RetainedBytes int        `json:"retained_bytes"`
	Truncated     bool       `json:"truncated"`
}

type BackgroundProcessOutput struct {
	ID        string `json:"id"`
	Output    string `json:"output"`
	Cursor    int64  `json:"cursor"`
	Reset     bool   `json:"reset"`
	Running   bool   `json:"running"`
	ExitCode  int    `json:"exit_code"`
	ExitError string `json:"exit_error,omitempty"`
	Truncated bool   `json:"truncated"`
}

func (c *Client) ListBackgroundProcesses(ctx context.Context) ([]BackgroundProcess, error) {
	var out []BackgroundProcess
	err := c.doJSON(ctx, http.MethodGet, "/processes", nil, &out)
	return out, err
}

func (c *Client) GetBackgroundProcessOutput(ctx context.Context, id string, cursor *int64) (BackgroundProcessOutput, error) {
	path := "/processes/" + url.PathEscape(id) + "/output"
	if cursor != nil {
		path += "?cursor=" + strconv.FormatInt(*cursor, 10)
	}
	var out BackgroundProcessOutput
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
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
	c.authorize(req)

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
