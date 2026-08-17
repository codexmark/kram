package app

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
)

type sessionsListMsg struct {
	sessions []daemonclient.Session
	err      error
}

func listSessionsCmd(c *daemonclient.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sessions, err := c.ListSessions(ctx)
		return sessionsListMsg{sessions: sessions, err: err}
	}
}

type sessionCreatedMsg struct {
	session daemonclient.Session
	err     error
}

func createSessionCmd(c *daemonclient.Client, title string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sess, err := c.CreateSession(ctx, title)
		return sessionCreatedMsg{session: sess, err: err}
	}
}

type historyLoadedMsg struct {
	session  daemonclient.Session
	messages []daemonclient.Message
	err      error
}

func loadHistoryCmd(c *daemonclient.Client, sessionID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sess, msgs, err := c.GetSession(ctx, sessionID)
		return historyLoadedMsg{session: sess, messages: msgs, err: err}
	}
}

// streamStartMsg reports whether the connection for a new agent turn was
// opened successfully. The turn itself hasn't produced anything yet —
// events arrive one at a time via streamEventMsg/readNextEventCmd.
type streamStartMsg struct {
	stream *daemonclient.MessageStream
	err    error
}

func startSendMessageCmd(c *daemonclient.Client, sessionID, content string) tea.Cmd {
	return func() tea.Msg {
		// No fixed timeout here: a multi-tool agent turn can legitimately
		// run long. The connection is torn down when the program quits or
		// the stream reports done/error.
		stream, err := c.SendMessageStream(context.Background(), sessionID, content, nil)
		return streamStartMsg{stream: stream, err: err}
	}
}

// streamEventMsg is one event off an open MessageStream. done mirrors
// MessageStream.Next's contract: once true, the caller stops reading.
type streamEventMsg struct {
	stream *daemonclient.MessageStream
	event  daemonclient.StreamEvent
	done   bool
	err    error
}

func readNextEventCmd(stream *daemonclient.MessageStream) tea.Cmd {
	return func() tea.Msg {
		evt, done, err := stream.Next()
		return streamEventMsg{stream: stream, event: evt, done: done, err: err}
	}
}

type statusResultMsg struct {
	status statusclient.Status
	err    error
}

func fetchStatusCmd(c *statusclient.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err := c.Fetch(ctx)
		return statusResultMsg{status: st, err: err}
	}
}

type contextResultMsg struct {
	usage daemonclient.ContextUsage
	err   error
}

func fetchContextCmd(c *daemonclient.Client, sessionID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		usage, err := c.GetContext(ctx, sessionID)
		return contextResultMsg{usage: usage, err: err}
	}
}

// animTickMsg drives the footer's breathing dot and sparkline while a
// request is in flight. It reschedules itself only while waiting — once
// the real response lands, the footer settles on real telemetry instead.
type animTickMsg time.Time

func animTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return animTickMsg(t) })
}
