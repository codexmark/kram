package app

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
)

// pendingQuestion is an in-flight ask_question call: the agent turn is
// genuinely paused server-side (the daemon's HTTP handler is blocked
// inside the tool call) until AnswerQuestion lands, so this replaces the
// normal chat input until answered — there's no "type something else in
// the meantime" state here, matching how blocking clarification tools
// work in opencode/OpenClaude/Hermes alike.
type pendingQuestion struct {
	id       string
	question string
	options  []string
	cursor   int
}

// renderQuestion draws the question in place of the normal input box:
// an up/down-selectable list if the tool provided options, or a plain
// text prompt otherwise.
func (m Model) renderQuestion() string {
	var b strings.Builder
	b.WriteString(styleBadgeWarn.Render("? ") + styleBody.Render(m.question.question) + "\n")

	if len(m.question.options) == 0 {
		b.WriteString(m.questionInput.View())
		return b.String()
	}

	for i, opt := range m.question.options {
		if i == m.question.cursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(opt) + "\n")
		} else {
			b.WriteString("  " + opt + "\n")
		}
	}
	b.WriteString(styleHint.Render("↑↓ escolher · enter responder"))
	return b.String()
}

func (m Model) handleQuestionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	q := m.question

	if len(q.options) == 0 {
		if msg.String() == "enter" {
			answer := strings.TrimSpace(m.questionInput.Value())
			id := q.id
			m.question = nil
			m.questionInput.SetValue("")
			m.refreshTranscript()
			return m, answerQuestionCmd(m.daemon, m.sessionID, id, answer)
		}
		var cmd tea.Cmd
		m.questionInput, cmd = m.questionInput.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "up":
		if q.cursor > 0 {
			q.cursor--
		}
	case "down":
		if q.cursor < len(q.options)-1 {
			q.cursor++
		}
	case "enter":
		answer := q.options[q.cursor]
		id := q.id
		m.question = nil
		m.refreshTranscript()
		return m, answerQuestionCmd(m.daemon, m.sessionID, id, answer)
	}
	return m, nil
}

type answerSentMsg struct{ err error }

func answerQuestionCmd(c *daemonclient.Client, sessionID, questionID, answer string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := c.AnswerQuestion(ctx, sessionID, questionID, answer)
		return answerSentMsg{err: err}
	}
}
