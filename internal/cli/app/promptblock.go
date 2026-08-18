package app

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
)

// promptBlockMaxWidthPct/promptBlockMinWidth/promptBlockNarrowTerm bound
// the block's width: up to ~68% of the terminal on a wide one, degrading
// to near-full-width with small margins once the terminal itself is
// narrow — see DECISIONS.md, "Right-aligned prompt block."
const (
	promptBlockMaxWidthPct = 68
	promptBlockMinWidth    = 20
	promptBlockNarrowTerm  = 50
)

// renderPromptBlock renders the user's own message as a right-aligned,
// asymmetric block — deliberately NOT a chat bubble: no full border, no
// tail, no background card spanning the width. Just a right-aligned
// column with a colored vertical accent on its left edge and a small
// "you" label above it, so a user turn is visually distinct at a glance
// without the transcript reading like a messaging app. This replaces an
// earlier two-sided-bubble attempt that read worse (see DECISIONS.md,
// "One left-aligned column... — reversed decision") — the fix here is
// narrower: an exception for the user's own prompt specifically, not a
// return to two columns.
//
// The block's width shrinks to fit short content rather than always
// stretching to the wrap limit — deliberately avoiding lipgloss's
// Style.Width(), which pads short content with trailing spaces (a real
// bug this codebase already hit once measuring a padded string's width
// instead of its content's, see DECISIONS.md). Alignment here is done by
// hand against each line's actual rendered width instead.
func (m Model) renderPromptBlock(content string) string {
	width := m.width
	if width <= 0 {
		width = 80
	}

	contentWidth := width * promptBlockMaxWidthPct / 100
	if width < promptBlockNarrowTerm || contentWidth < promptBlockMinWidth {
		contentWidth = width - 2 // narrow terminal: near-full-width, small margin
	}
	if contentWidth < 1 {
		contentWidth = 1
	}

	wrapped := strings.TrimRight(wordwrap.String(content, contentWidth), "\n")
	lines := strings.Split(wrapped, "\n")

	blockWidth := 0
	for _, l := range lines {
		if w := lipgloss.Width(l); w > blockWidth {
			blockWidth = w
		}
	}

	const bar = "▐ "
	leftMargin := width - blockWidth - lipgloss.Width(bar)
	if leftMargin < 0 {
		leftMargin = 0
	}
	margin := strings.Repeat(" ", leftMargin)

	var b strings.Builder
	labelPad := width - lipgloss.Width("you")
	if labelPad < 0 {
		labelPad = 0
	}
	b.WriteString(strings.Repeat(" ", labelPad) + styleHint.Render("you") + "\n")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(margin + styleBadgeAccent.Render(bar) + styleUserBody.Render(l))
	}
	return b.String()
}
