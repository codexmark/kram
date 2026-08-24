package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// refreshTranscript rebuilds the viewport content from m.messages. It keeps
// following new output only while the viewport is already at the bottom; a
// user who deliberately scrolled up must not be snapped back down by the
// thinking animation or a streaming delta.
func (m *Model) refreshTranscript() {
	followBottom := m.viewport.AtBottom()
	previousOffset := m.viewport.YOffset
	var b strings.Builder
	m.processLinkRows = make(map[int]string)
	if len(m.messages) == 0 && m.wizardWelcomeSession {
		m.viewport.SetContent(m.renderWizardWelcomeBanner())
		if followBottom {
			m.viewport.GotoBottom()
		} else {
			m.viewport.SetYOffset(previousOffset)
		}
		return
	}
	for i, msg := range m.messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		switch msg.Role {
		case "user":
			// User text is never run through the markdown renderer — it's
			// what was typed, not a formatted reply, and echoing it back
			// reformatted would be surprising. Right-aligned prompt block,
			// not a chat bubble — see promptblock.go and DECISIONS.md.
			b.WriteString(m.renderPromptBlock(msg.Content))
		default:
			for _, act := range msg.ToolActivity {
				row := strings.Count(b.String(), "\n")
				if act.ProcessID != "" {
					m.processLinkRows[row] = act.ProcessID
				}
				b.WriteString(m.renderToolActivity(act) + "\n")
			}
			// While a turn is live, durable notices belong above the activity
			// surface. Rendering them below it makes the K stop being the visual
			// tail of the run (the segment-notice bug caught this live). Once the
			// turn is complete, the conventional answer-then-notices order is fine.
			if msg.streaming {
				for _, n := range msg.Notices {
					b.WriteString(styleHint.Render("· "+n) + "\n")
				}
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
				b.WriteString(styleKramTag.Render("kram") + "  " + styleBody.Render(msg.Content) + "\n" + m.thinkingLine())
			case msg.Content != "":
				b.WriteString(styleKramTag.Render("kram") + "  " + renderMarkdown(m.mdRenderer, msg.Content))
			}
			if !msg.streaming {
				for _, n := range msg.Notices {
					b.WriteString("\n" + styleHint.Render("· "+n))
				}
				if row := renderFilesTouched(touchedFiles(msg.ToolActivity)); row != "" {
					b.WriteString("\n" + row)
				}
			}
		}
	}
	if m.err != nil {
		b.WriteString("\n\n" + styleErrBadge.Render("erro: "+m.err.Error()))
	}
	m.viewport.SetContent(b.String())
	if followBottom {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(previousOffset)
	}
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

// thinkingLine is the animated activity surface shown while the agent loop is
// running. Its labels come from real route/delta/tool/heartbeat events; none is
// model-generated narration, so making the UI lively costs zero tokens and
// never claims knowledge Kram does not have. Past stallThreshold it names the
// transport symptom rather than the ambiguous "sem resposta".
func (m Model) thinkingLine() string {
	elapsed := time.Since(m.waitStartedAt).Round(time.Second)
	stalled := !m.waitStartedAt.IsZero() && time.Since(m.lastEventAt) > stallThreshold
	indicator := renderThinkingK(m.animFrame, stalled)
	label := m.activityLabel()
	rail := m.renderActivityRail(stalled)
	meta := elapsed.String()
	if m.workState == workToolActive && !m.toolStartedAt.IsZero() {
		meta += " · tool " + time.Since(m.toolStartedAt).Round(time.Second).String()
	}
	if m.heartbeats > 0 && !stalled {
		meta += fmt.Sprintf(" · pulso %d", m.heartbeats)
	}
	if m.segments > 1 {
		meta += fmt.Sprintf(" · segmento %d/%d", m.segment, m.segments)
	}
	if stalled {
		sinceEvent := time.Since(m.lastEventAt).Round(time.Second)
		label = "CONEXÃO SEM EVENTOS"
		meta = fmt.Sprintf("há %s · total %s", sinceEvent, elapsed)
		return indicator + "  " + styleBadgeWarn.Bold(true).Render(label) + "  " + rail + "  " + styleBadgeWarn.Render(meta)
	}
	return indicator + "  " + shimmerText(label, m.animFrame) + "  " + rail + "  " + styleMeta.Render(meta)
}

func (m Model) activityLabel() string {
	switch m.workState {
	case workModelActive:
		return "MODELO ATIVO"
	case workToolActive:
		if m.activeTool != "" {
			return "EXECUTANDO · " + m.activeTool
		}
		return "EXECUTANDO TOOL"
	case workAnalyzingResult:
		return "ANALISANDO RESULTADO"
	case workWriting:
		return "ESCREVENDO"
	default:
		return "PREPARANDO ROTA"
	}
}

func (m Model) renderActivityRail(stalled bool) string {
	const nodes = 7
	active := positiveModulo(m.animFrame/2, nodes)
	var b strings.Builder
	for i := 0; i < nodes; i++ {
		glyph := "━"
		style := styleFaintTrack
		if i == active {
			glyph = "●"
			style = styleBadgeAccent.Bold(true)
			if stalled {
				style = styleBadgeWarn.Bold(true)
			}
		}
		b.WriteString(style.Render(glyph))
	}
	return "◜" + b.String() + "◝"
}

func (m Model) View() string {
	if !m.ready {
		return "iniciando…"
	}
	if m.phase == phaseSplash {
		return m.renderBootSplash()
	}
	if m.phase == phasePicker {
		return m.renderPicker()
	}
	if m.phase == phaseAccounts {
		return m.renderAccounts()
	}
	if m.phase == phaseTools {
		return m.renderToolsToggle()
	}
	if m.phase == phaseWizardEnvironment {
		return m.renderWizardEnvironment()
	}
	if m.phase == phaseWizardProjects {
		return m.renderWizardProjects()
	}
	if m.phase == phaseWizardRouting {
		return m.renderWizardRouting()
	}
	if m.phase == phaseWizardPermissions {
		return m.renderWizardPermissions()
	}
	if m.phase == phaseWizardToolsPreset {
		return m.renderWizardToolsPreset()
	}
	if m.phase == phaseWizardSystemCheck {
		return m.renderWizardSystemCheck()
	}
	if m.phase == phaseWizardSummary {
		return m.renderWizardSummary()
	}

	var b strings.Builder
	b.WriteString(m.renderRouteBar())
	b.WriteString("\n")
	body := m.viewport.View()
	if m.active == panelProcesses {
		pane := m.renderProcessPane(m.viewport.Height, m.processPaneWidth())
		if m.processUsesTile() {
			body = lipgloss.JoinHorizontal(lipgloss.Top, body, styleHint.Render("│"), pane)
		} else {
			body = pane
		}
	}
	b.WriteString(body)
	b.WriteString("\n")
	switch {
	case m.question != nil:
		b.WriteString(m.renderQuestion())
	case m.approval != nil:
		b.WriteString(m.renderApproval())
	default:
		b.WriteString(m.input.View())
	}
	b.WriteString("\n")

	switch m.active {
	case panelStrategy:
		b.WriteString(m.renderStrategyPanel())
	case panelStrategyPicker:
		b.WriteString(m.renderStrategyPicker())
	case panelContext:
		b.WriteString(m.renderContextPanel())
	case panelRoute:
		b.WriteString(m.renderRoutePanel())
	}
	b.WriteString(m.renderFooter())

	return m.clipboardSequence + b.String()
}

// renderFooter draws the pulse bar: a breathing dot for the active
// provider, latency, and fallback trail — that's now the route bar's job
// (see routebar.go), live during the turn and left showing the last
// completed model call's real story afterward. The footer is one line:
// running token totals on the left, the context-usage icon and keyboard
// shortcuts on the right. See DECISIONS.md, "Footer stops duplicating
// the route bar."
func (m Model) renderFooter() string {
	tokens := ""
	if m.strategyNotice != "" {
		tokens = styleBadgeOK.Render(m.strategyNotice)
	} else if m.copyNotice != "" {
		tokens = styleBadgeOK.Render(m.copyNotice)
	} else if last := m.lastAssistantTokens(); last != "" {
		tokens = styleMeta.Render(last)
	}
	return padBetween(m.width, tokens, m.footerRightBlock())
}

// footerRightBlock is the clickable context-usage icon plus keyboard
// hints, right-aligned on the footer's row. It's a method (not inlined)
// because handleMouse needs the exact same string to compute where the
// click target starts.
func (m Model) footerRightBlock() string {
	return joinNonEmpty("  ", m.contextIcon(), styleHint.Render("^b processos"),
		styleHint.Render("^r rota"), styleHint.Render("^t contexto"), styleHint.Render("^s estratégia"), styleHint.Render("^p detalhes"))
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
	label := act.Name + "(" + args + ")"
	if act.ProcessID != "" {
		label = styleBadgeAccent.Render(act.ProcessID) + " " + styleMeta.Render(label)
	} else {
		label = styleMeta.Render(label)
	}
	return styleHint.Render("  ↳ ") + label + " " + mark
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
