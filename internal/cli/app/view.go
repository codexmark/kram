package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
)

// refreshTranscript rebuilds the viewport content from m.messages and
// scrolls to the bottom — called any time messages change.
func (m *Model) refreshTranscript() {
	var b strings.Builder
	for i, msg := range m.messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		switch msg.Role {
		case "user":
			// User text is never run through the markdown renderer — it's
			// what was typed, not a formatted reply, and echoing it back
			// reformatted would be surprising. Anchored to the right, the
			// basic chat convention this was missing — the agent's replies
			// stay on the left below.
			b.WriteString(m.renderUserBubble(msg.Content))
		default:
			for _, act := range msg.ToolActivity {
				b.WriteString(renderToolActivity(act) + "\n")
			}
			if msg.Content != "" {
				b.WriteString(styleKramTag.Render("kram") + "  " + renderMarkdown(m.mdRenderer, msg.Content))
			}
			for _, n := range msg.Notices {
				b.WriteString("\n" + styleHint.Render("· "+n))
			}
		}
	}
	if m.waiting {
		if m.messages != nil {
			b.WriteString("\n\n")
		}
		b.WriteString(m.thinkingLine())
	}
	if m.err != nil {
		b.WriteString("\n\n" + styleErrBadge.Render("erro: "+m.err.Error()))
	}
	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
}

// thinkingPalette is the same breathing-dot idea the footer's pulse bar
// uses (see footer_helpers.go), reused here so the transcript's "working"
// state reads as the same visual language rather than a second, unrelated
// spinner style.
var thinkingPalette = []lipgloss.Style{styleBadgeIdle, styleBadgeAccent, styleBadgeOK, styleBadgeAccent}

// thinkingLine is the animated placeholder shown in the transcript while
// the agent loop is running — a breathing "kram" tag (color cycling
// through the same palette as the footer) plus the spinner and a
// build-up ellipsis, driven by animFrame so it's in lockstep with the
// footer's own animation.
func (m Model) thinkingLine() string {
	tagStyle := thinkingPalette[(m.animFrame/3)%len(thinkingPalette)].Bold(true)
	dots := strings.Repeat(".", 1+(m.animFrame/2)%3)
	return tagStyle.Render("kram") + "  " + m.spin.View() + " " + styleMeta.Render("pensando"+dots)
}

// renderUserBubble right-aligns a user message — the "you" tag and
// content wrapped to a bubble narrower than the full width, then the
// whole block pushed flush right, so the transcript reads as a normal
// two-sided chat (you on the right, kram on the left) instead of
// everything stacked in one left-aligned column.
func (m Model) renderUserBubble(content string) string {
	bubbleWidth := m.viewport.Width - 10
	if bubbleWidth > 64 {
		bubbleWidth = 64
	}
	if bubbleWidth < 20 {
		bubbleWidth = m.viewport.Width
	}
	wrappedContent := lipgloss.NewStyle().Width(bubbleWidth).Render(styleBody.Render(content))
	block := styleYouTag.Render("you") + "\n" + wrappedContent
	return lipgloss.NewStyle().Width(m.viewport.Width).Align(lipgloss.Right).Render(block)
}

func (m Model) View() string {
	if !m.ready {
		return "iniciando…"
	}
	if m.phase == phasePicker {
		return m.renderPicker()
	}

	var b strings.Builder
	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	b.WriteString(m.input.View())
	b.WriteString("\n")

	switch m.active {
	case panelStrategy:
		b.WriteString(m.renderStrategyPanel())
	case panelContext:
		b.WriteString(m.renderContextPanel())
	}
	b.WriteString(m.renderFooter())

	return b.String()
}

// renderFooter draws the pulse bar: a breathing dot for the active
// provider with an animated latency sparkline while a request is in
// flight, and — once it lands — the real per-request fallback trail this
// exact reply took, plus running token totals. It never grows past two
// lines.
func (m Model) renderFooter() string {
	line1 := m.footerLine1()
	line2 := m.footerLine2()
	return line1 + "\n" + line2
}

func (m Model) footerLine1() string {
	var dot, name, latency, spark string

	if m.waiting {
		dot = styleBadgeWarn.Render("●")
		name = styleBody.Render(m.combo)
		latency = styleMeta.Render("…")
		spark = animatedSparkline(m.animFrame)
	} else if m.lastProvider != "" {
		dot = styleBadgeOK.Render("●")
		name = styleBody.Render(m.lastProvider)
		if ms := lastLatencyMS(m.lastAttempts, m.lastProvider); ms >= 0 {
			latency = styleMeta.Render(fmt.Sprintf("%dms", ms))
		}
		spark = staticSparkline(m.lastAttempts)
	} else {
		dot = styleBadgeIdle.Render("●")
		name = styleMeta.Render(m.combo)
	}

	tokens := ""
	if last := m.lastAssistantTokens(); last != "" {
		tokens = styleMeta.Render(last)
	}

	left := joinNonEmpty("  ", dot, name, spark, latency)
	return padBetween(m.width, left, tokens)
}

func (m Model) footerLine2() string {
	trail := attemptTrailGlyphs(m.lastAttempts)
	count := ""
	if n := len(m.lastAttempts); n > 0 {
		word := "tentativa"
		if n != 1 {
			word = "tentativas"
		}
		count = styleMeta.Render(fmt.Sprintf("%d %s", n, word))
	}
	left := joinNonEmpty("  ", trail, count)
	return padBetween(m.width, left, m.footerRightBlock())
}

// footerRightBlock is the clickable context-usage icon plus keyboard
// hints, right-aligned on the footer's bottom row. It's a method (not
// inlined) because handleMouse needs the exact same string to compute
// where the click target starts.
func (m Model) footerRightBlock() string {
	return joinNonEmpty("  ", m.contextIcon(), styleHint.Render("^t contexto"), styleHint.Render("^p estratégia"))
}

// contextIcon is the discreet, clickable context-window badge: a filled
// dot whose color reflects real usage (from the daemon's own compaction
// threshold — see internal/daemon/compaction) plus a percentage. Opens
// the context panel on click or ^t.
func (m Model) contextIcon() string {
	if !m.haveContext || m.contextData.Budget <= 0 {
		return styleBadgeIdle.Render("◔ …")
	}
	pct := m.contextData.Used * 100 / m.contextData.Budget
	style := styleBadgeOK
	switch {
	case pct >= 90:
		style = styleBadgeBad
	case pct >= 70:
		style = styleBadgeWarn
	}
	return style.Render(fmt.Sprintf("◔ %d%%", pct))
}

// renderToolActivity draws one line per tool call the agent loop made,
// between the user's message and the final answer — real activity, not a
// generic "thinking" placeholder.
func renderToolActivity(act daemonclient.ToolActivity) string {
	args := act.Args
	if len(args) > 60 {
		args = args[:60] + "…"
	}
	mark := styleBadgeOK.Render("✓")
	if !act.OK {
		mark = styleBadgeBad.Render("✗")
	}
	return styleHint.Render("  ↳ ") + styleMeta.Render(act.Name+"("+args+")") + " " + mark
}

func joinNonEmpty(sep string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// padBetween places left and right on one line, right-aligned, without
// overflowing the terminal width.
func padBetween(width int, left, right string) string {
	if width <= 0 {
		return left
	}
	lw := lipgloss.Width(left)
	rw := lipgloss.Width(right)
	gap := width - lw - rw
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}
