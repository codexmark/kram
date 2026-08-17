package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
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
			b.WriteString(styleYouTag.Render("you") + "  " + styleBody.Render(msg.Content))
		default:
			b.WriteString(styleKramTag.Render("kram") + "  " + styleBody.Render(msg.Content))
		}
	}
	if m.waiting {
		if m.messages != nil {
			b.WriteString("\n\n")
		}
		b.WriteString(styleMeta.Render(m.spin.View() + " pensando…"))
	}
	if m.err != nil {
		b.WriteString("\n\n" + styleErrBadge.Render("erro: "+m.err.Error()))
	}
	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
}

func (m Model) View() string {
	if !m.ready {
		return "iniciando…"
	}

	var b strings.Builder
	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	b.WriteString(m.input.View())
	b.WriteString("\n")

	if m.panelOpen {
		b.WriteString(m.renderPanel())
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
	hint := styleHint.Render("^p estratégia")
	return padBetween(m.width, left, hint)
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
