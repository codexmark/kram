package app

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
)

// pendingApproval is an in-flight permission-policy approval: like
// pendingQuestion, the turn is genuinely paused server-side until answered.
// Distinct type (not reused pendingQuestion) because the two mean different
// things to the user — a question is the agent asking for information it's
// missing, an approval is the agent telling you exactly what it's about to
// do and waiting for permission — and because an approval's option set is
// always the fixed once/always/deny triad, never free text.
type pendingApproval struct {
	id      string
	tool    string
	subject string
	options []string // always ["once", "always", "deny"], but read from the event rather than hardcoded twice
	cursor  int
}

// renderApproval draws the pending approval in place of the normal input
// box — what tool, with what argument, waiting for once/always/deny.
func (m Model) renderApproval() string {
	var b strings.Builder
	summary := m.approval.tool
	if m.approval.subject != "" {
		summary += ": " + m.approval.subject
	}
	b.WriteString(styleBadgeWarn.Render("⚠ approval needed ") + styleBody.Render(summary) + "\n")

	for i, opt := range m.approval.options {
		if i == m.approval.cursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(opt) + "\n")
		} else {
			b.WriteString("  " + opt + "\n")
		}
	}
	b.WriteString(styleHint.Render("↑↓ escolher · enter confirmar"))
	return b.String()
}

func (m Model) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a := m.approval

	switch msg.String() {
	case "up":
		if a.cursor > 0 {
			a.cursor--
		}
	case "down":
		if a.cursor < len(a.options)-1 {
			a.cursor++
		}
	case "enter":
		decision := a.options[a.cursor]
		id := a.id
		m.approval = nil
		m.refreshTranscript()
		return m, answerApprovalCmd(m.daemon, m.sessionID, id, decision)
	}
	return m, nil
}

func answerApprovalCmd(c *daemonclient.Client, sessionID, approvalID, decision string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := c.AnswerApproval(ctx, sessionID, approvalID, decision)
		return answerSentMsg{err: err}
	}
}
