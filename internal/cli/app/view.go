package app

import (
	"fmt"
	"strings"
	"time"

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
				b.WriteString(m.renderToolActivity(act) + "\n")
			}
			switch {
			case msg.streaming && msg.Content == "":
				// Nothing generated yet this turn (still deciding, or
				// mid-tool-call) — the breathing placeholder.
				b.WriteString(m.thinkingLine())
			case msg.streaming:
				// Content is arriving live: plain text only. Markdown
				// parsed against an incomplete string (an unclosed code
				// fence, a stray "**") would flicker through broken
				// formatting every frame — the full render happens once,
				// below, when the message is complete.
				b.WriteString(styleKramTag.Render("kram") + "  " + styleBody.Render(msg.Content))
			case msg.Content != "":
				b.WriteString(styleKramTag.Render("kram") + "  " + renderMarkdown(m.mdRenderer, msg.Content))
			}
			for _, n := range msg.Notices {
				b.WriteString("\n" + styleHint.Render("· "+n))
			}
		}
	}
	if m.err != nil {
		b.WriteString("\n\n" + styleErrBadge.Render("erro: "+m.err.Error()))
	}
	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
}

// stallThreshold is how long without any event (delta, tool_start,
// tool_result, notice) before the "working" indicator stops implying
// steady progress and admits it might be stuck. This is a real signal
// (time since the last byte actually arrived), not a guess — the visual
// research pass that motivated this (OpenClaude's useStalledAnimation)
// found this distinction matters: an app that looks identically "busy"
// whether it's making progress or hung reads as broken once someone
// notices, and a plain spinner can't tell the two apart.
const stallThreshold = 8 * time.Second

// thinkingLine is the animated placeholder shown in the transcript while
// the agent loop is running: a genuine color-gradient shimmer across the
// "kram" tag (see shimmer.go — go-colorful interpolation, not a discrete
// palette step) plus a live elapsed-time counter, driven by animFrame so
// it's in lockstep with the footer's own animation. Past stallThreshold
// with no new event, it switches to a distinct warm color and says so
// plainly instead of continuing to shimmer as if nothing were wrong.
func (m Model) thinkingLine() string {
	elapsed := time.Since(m.waitStartedAt).Round(time.Second)

	if !m.waitStartedAt.IsZero() && time.Since(m.lastEventAt) > stallThreshold {
		return styleBadgeWarn.Bold(true).Render("kram") + "  " + m.spin.View() + " " +
			styleBadgeWarn.Render(fmt.Sprintf("ainda trabalhando… (%s sem resposta)", elapsed))
	}

	return shimmerText("kram", m.animFrame) + "  " + m.spin.View() + " " +
		styleMeta.Render(fmt.Sprintf("pensando (%s)", elapsed))
}

// renderUserBubble right-aligns a user message — the "you" tag and
// content wrapped to a bubble narrower than the full width, then the
// whole block pushed flush right, so the transcript reads as a normal
// two-sided chat (you on the right, kram on the left) instead of
// everything stacked in one left-aligned column.
//
// Padding is computed by hand, per line, rather than leaning on lipgloss's
// Style.Width+Align: that combination pads a block out to a fixed box
// only when its content actually reaches that width on some line — a
// short message (or the lone "you" tag line) has no line that long, so
// nothing pads it and it renders hugging the left edge instead. Measuring
// each line's real rendered width and left-padding it individually always
// anchors the block flush right, regardless of content length.
func (m Model) renderUserBubble(content string) string {
	bubbleWidth := m.viewport.Width - 10
	if bubbleWidth > 64 {
		bubbleWidth = 64
	}
	if bubbleWidth < 20 {
		bubbleWidth = m.viewport.Width
	}

	// Width() wraps long lines at bubbleWidth, but it also pads short
	// ones out to bubbleWidth with trailing spaces (left-aligned inside
	// that box by default) — trimmed back off below, since otherwise the
	// per-line pad computed next would measure the padded box width
	// instead of the real content, and the visible text would sit at the
	// box's left edge rather than flush against the transcript's right
	// edge.
	wrapped := lipgloss.NewStyle().Width(bubbleWidth).Render(styleBody.Render(content))
	rawLines := strings.Split(wrapped, "\n")
	lines := make([]string, 0, len(rawLines)+1)
	lines = append(lines, styleYouTag.Render("you"))
	for _, l := range rawLines {
		lines = append(lines, strings.TrimRight(l, " "))
	}

	rightAligned := make([]string, len(lines))
	for i, line := range lines {
		pad := m.viewport.Width - lipgloss.Width(line)
		if pad < 0 {
			pad = 0
		}
		rightAligned[i] = strings.Repeat(" ", pad) + line
	}
	return strings.Join(rightAligned, "\n")
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
		latency = styleMeta.Render(time.Since(m.waitStartedAt).Round(time.Second).String())
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
func (m Model) renderToolActivity(act daemonclient.ToolActivity) string {
	args := act.Args
	if len(args) > 60 {
		args = args[:60] + "…"
	}
	mark := m.spin.View() // still running: real-time spinner, not a guessed outcome
	if !act.Running {
		mark = styleBadgeOK.Render("✓")
		if !act.OK {
			mark = styleBadgeBad.Render("✗")
		}
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
