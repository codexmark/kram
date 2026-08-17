package app

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
)

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

type sendResultMsg struct {
	result daemonclient.SendMessageResult
	err    error
}

func sendMessageCmd(c *daemonclient.Client, sessionID, content string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		res, err := c.SendMessage(ctx, sessionID, content)
		return sendResultMsg{result: res, err: err}
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

// animTickMsg drives the footer's breathing dot and sparkline while a
// request is in flight. It reschedules itself only while waiting — once
// the real response lands, the footer settles on real telemetry instead.
type animTickMsg time.Time

func animTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return animTickMsg(t) })
}
