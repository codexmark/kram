package app

import (
	"context"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
	"github.com/codexmark/kram-gateway/internal/credentials"
	"github.com/codexmark/kram-gateway/internal/providercatalog"
	"github.com/codexmark/kram-gateway/internal/providerping"
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

// pingResultsMsg carries one real connectivity/auth check per account,
// keyed by EnvVar — see providerping.Ping. An account this run had no
// resolvable key for (never configured at all) is simply absent from the
// map, not reported as down.
type pingResultsMsg struct {
	results map[string]providerping.Result
}

// pingAccountsCmd checks every catalog account concurrently — this runs
// on demand (entering the accounts screen, or a manual refresh), never in
// the background, and each check is bounded by providerping's own
// timeout, so the whole batch can't hang the UI indefinitely. credStore
// may be nil (a fresh install with no credentials file yet); the real env
// var always wins over a stored key, same precedence cmd/kram's own
// startup wiring uses.
func pingAccountsCmd(credStore *credentials.Store) tea.Cmd {
	return func() tea.Msg {
		var wg sync.WaitGroup
		var mu sync.Mutex
		results := make(map[string]providerping.Result, len(providercatalog.Accounts))

		for _, acct := range providercatalog.Accounts {
			key := os.Getenv(acct.EnvVar)
			if key == "" && credStore != nil {
				key = credStore.Get(acct.EnvVar)
			}
			if key == "" {
				continue // nothing configured for this account — not "down", just unchecked
			}
			kind, baseURL, ok := providerKindForEnvVar(acct.EnvVar)
			if !ok {
				continue
			}

			wg.Add(1)
			go func(envVar, kind, baseURL, key string) {
				defer wg.Done()
				res := providerping.Ping(context.Background(), kind, baseURL, key)
				mu.Lock()
				results[envVar] = res
				mu.Unlock()
			}(acct.EnvVar, kind, baseURL, key)
		}

		wg.Wait()
		return pingResultsMsg{results: results}
	}
}

// providerKindForEnvVar finds the adapter kind/base URL to use for
// pinging an account — providercatalog.Accounts is the deduplicated
// one-row-per-credential view, but pinging needs the same Kind/BaseURL
// providercatalog.Providers (one-row-per-combo-entry) carries; several
// Provider entries can share one Account (OpenRouter's three free-model
// entries), so the first match is representative — they all use the same
// key against the same host.
func providerKindForEnvVar(envVar string) (kind, baseURL string, ok bool) {
	for _, p := range providercatalog.Providers {
		if p.EnvVar == envVar {
			return p.Kind, p.BaseURL, true
		}
	}
	return "", "", false
}

// animTickMsg drives the footer's breathing dot and sparkline while a
// request is in flight. It reschedules itself only while waiting — once
// the real response lands, the footer settles on real telemetry instead.
type animTickMsg time.Time

func animTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return animTickMsg(t) })
}
