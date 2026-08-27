package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// approvalDiffMaxRows bounds the diff viewport's height so a large edit's
// diff scrolls in place instead of pushing the once/always/deny buttons off
// screen. See colorizeUnifiedDiff and handleApprovalKey's scroll keys.
const approvalDiffMaxRows = 16

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
	// diff is the unified diff of an edit_file/write_file change, empty for
	// tools with no reviewable change; diffVP windows and scrolls it so a
	// big diff never pushes the option buttons off screen.
	diff   string
	diffVP viewport.Model
}

// newPendingApproval builds the approval state from a stream event, sizing
// and colorizing the diff viewport when the event carries one.
func newPendingApproval(evt daemonclient.StreamEvent, width int) *pendingApproval {
	a := &pendingApproval{id: evt.ApprovalID, tool: evt.Tool, subject: evt.Subject, options: evt.Options, diff: evt.Diff}
	if a.diff != "" {
		// Size to the diff, capped: a small edit occupies only the rows it
		// needs (no dead padding above the buttons), while a large diff caps
		// at approvalDiffMaxRows and scrolls.
		rows := minInt(strings.Count(a.diff, "\n")+1, approvalDiffMaxRows)
		a.diffVP = viewport.New(maxInt(1, width-2), maxInt(1, rows))
		a.diffVP.SetContent(colorizeUnifiedDiff(a.diff))
	}
	return a
}

// renderApproval draws the pending approval in place of the normal input
// box — what tool, with what argument, the change as a scrollable colored
// unified diff when there is one, and the once/always/deny options.
func (m Model) renderApproval() string {
	var b strings.Builder
	summary := m.approval.tool
	if m.approval.subject != "" {
		summary += ": " + m.approval.subject
	}
	b.WriteString(styleBadgeWarn.Render("⚠ approval needed ") + styleBody.Render(summary) + "\n")

	if m.approval.diff != "" {
		b.WriteString(styleHint.Render(fmt.Sprintf("── diff · %s ──", m.approval.subject)) + "\n")
		b.WriteString(m.approval.diffVP.View() + "\n")
	}

	// "always" on a file edit persists a grant for the whole path (any
	// future content, not just this diff) — say so, so the user isn't
	// giving blanket access to a file thinking they approved one change.
	fileTool := m.approval.tool == "edit_file" || m.approval.tool == "write_file"
	for i, opt := range m.approval.options {
		label := opt
		if fileTool && opt == "always" && m.approval.subject != "" {
			label = opt + styleHint.Render(" · allow all future edits to "+m.approval.subject)
		}
		if i == m.approval.cursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(label) + "\n")
		} else {
			b.WriteString("  " + label + "\n")
		}
	}
	hint := approvalHint
	if m.approval.diff != "" {
		hint += approvalDiffScrollHint
	}
	b.WriteString(styleHint.Render(hint))
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
	// Diff scroll — the main key switch is unreachable during an approval
	// (handleApprovalKey short-circuits it), so the scroll keys live here.
	case "pgup":
		a.diffVP.HalfViewUp()
	case "pgdown":
		a.diffVP.HalfViewDown()
	case "home":
		a.diffVP.GotoTop()
	case "end":
		a.diffVP.GotoBottom()
	case "enter":
		decision := a.options[a.cursor]
		id := a.id
		m.approval = nil
		m.refreshTranscript()
		return m, answerApprovalCmd(m.daemon, m.sessionID, id, decision)
	}
	return m, nil
}

type diffLineKind int

const (
	diffContext diffLineKind = iota
	diffAdd
	diffDel
	diffHeader
)

// classifyDiffLine categorizes one unified-diff line. inHunk says whether a
// "@@" hunk header has already been seen — which matters because the "---"/
// "+++" *file* headers only appear before the first hunk, whereas a deleted
// source line reading "--i;" is emitted as "---i;" and an added "++i;" as
// "+++i;". Classifying by leading-char-only once inside a hunk keeps those
// real changes colored as add/del instead of miscolored as headers. Returns
// the kind and the updated inHunk.
func classifyDiffLine(line string, inHunk bool) (diffLineKind, bool) {
	switch {
	case strings.HasPrefix(line, "@@"):
		return diffHeader, true
	case !inHunk && (strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") ||
		strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ")):
		return diffHeader, inHunk
	case strings.HasPrefix(line, "+"):
		return diffAdd, inHunk
	case strings.HasPrefix(line, "-"):
		return diffDel, inHunk
	default:
		return diffContext, inHunk
	}
}

// colorizeUnifiedDiff colors a unified diff line-by-line: additions green,
// deletions red, file/hunk headers dim-emphasized, context faint.
func colorizeUnifiedDiff(diff string) string {
	lines := strings.Split(diff, "\n")
	inHunk := false
	for i, line := range lines {
		var kind diffLineKind
		kind, inHunk = classifyDiffLine(line, inHunk)
		switch kind {
		case diffHeader:
			lines[i] = styleMeta.Render(line)
		case diffAdd:
			lines[i] = styleBadgeOK.Render(line)
		case diffDel:
			lines[i] = styleBadgeBad.Render(line)
		default:
			lines[i] = styleHint.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}

func answerApprovalCmd(c *daemonclient.Client, sessionID, approvalID, decision string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := c.AnswerApproval(ctx, sessionID, approvalID, decision)
		return answerSentMsg{err: err}
	}
}
