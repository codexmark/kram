package app

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// One-key rewind (Ctrl+G): restore the workspace to the newest automatic
// pre-mutation checkpoint the daemon took (see agent.AutoCheckpointPrefix).
// Deliberately a two-press flow — restoring is destructive, so the first
// press fetches and *shows* exactly what would be restored, and only a
// second press (pinned to that checkpoint's id, never "whatever is newest
// by then") executes it. Esc or the notice timeout disarms.

type rewindInfoMsg struct {
	checkpoint daemonclient.RewindCheckpoint
	err        error
}

type rewindDoneMsg struct {
	result daemonclient.RewindResult
	err    error
}

func rewindInfoCmd(c *daemonclient.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cp, err := c.RewindInfo(ctx)
		return rewindInfoMsg{checkpoint: cp, err: err}
	}
}

func rewindCmd(c *daemonclient.Client, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := c.Rewind(ctx, id)
		return rewindDoneMsg{result: res, err: err}
	}
}

func (m Model) handleRewindKey() (tea.Model, tea.Cmd) {
	if m.phase != phaseChat || m.rewindBusy {
		return m, nil
	}
	// Rewinding under a running turn would yank files out from under the
	// tools writing them — interrupt first, rewind second.
	if m.waiting {
		m.strategyNoticeRev++
		m.strategyNotice = rewindTurnRunningNotice
		return m, clearStrategyNoticeCmd(m.strategyNoticeRev)
	}
	if m.rewindArmed != nil {
		checkpoint := *m.rewindArmed
		m.rewindArmed = nil
		m.rewindBusy = true
		m.strategyNoticeRev++
		m.strategyNotice = rewindRestoringNotice
		return m, rewindCmd(m.daemon, checkpoint.ID)
	}
	m.rewindBusy = true
	return m, rewindInfoCmd(m.daemon)
}

func (m Model) handleRewindInfo(msg rewindInfoMsg) (tea.Model, tea.Cmd) {
	m.rewindBusy = false
	m.strategyNoticeRev++
	if msg.err != nil {
		m.strategyNotice = rewindNoCheckpointNotice
		return m, clearStrategyNoticeCmd(m.strategyNoticeRev)
	}
	cp := msg.checkpoint
	m.rewindArmed = &cp
	m.strategyNotice = fmt.Sprintf(rewindArmedNoticeFmt, cp.ShortID(), cp.CreatedAt.Format("15:04:05"))
	return m, clearStrategyNoticeCmd(m.strategyNoticeRev)
}

func (m Model) handleRewindDone(msg rewindDoneMsg) (tea.Model, tea.Cmd) {
	m.rewindBusy = false
	m.strategyNoticeRev++
	if msg.err != nil {
		m.strategyNotice = rewindFailedPrefix + msg.err.Error()
	} else {
		m.strategyNotice = fmt.Sprintf(rewindDoneNoticeFmt,
			len(msg.result.Restored.Changes), msg.result.Snapshot.ShortID())
	}
	return m, clearStrategyNoticeCmd(m.strategyNoticeRev)
}
