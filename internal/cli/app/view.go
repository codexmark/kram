package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// refreshTranscript rebuilds the viewport content from m.messages. It keeps
// following new output only while the viewport is already at the bottom; a
// user who deliberately scrolled up must not be snapped back down by the
// thinking animation or a streaming delta.
//
// Only the tail message can contain anything animFrame-dependent (the
// thinking line, or a running tool call's live spinner glyph — both only
// ever appear on the one message still streaming, since every earlier
// message's turn already finished and never changes again). So the
// static messages before it are rendered once here and cached in
// m.transcriptBody; refreshLiveIndicator (animTickMsg's own path, see
// its doc comment) re-renders only the small tail block on every
// animation frame instead of re-running this whole function — glamour
// re-rendering every past message, tool-activity re-rendering, etc. —
// which measured at ~16ms on an 80-message session, a third of the
// 50ms tick budget and the direct cause of a user-reported "travada"/
// slow-motion feel once the tick rate was raised for smoothness
// (issue #53) rather than the fix that was meant to be.
func (m *Model) refreshTranscript() {
	followBottom := m.viewport.AtBottom()
	previousOffset := m.viewport.YOffset
	if len(m.messages) == 0 && m.wizardWelcomeSession {
		m.transcriptLiveIndicatorActive = false
		m.viewport.SetContent(m.renderWizardWelcomeBanner())
		if followBottom {
			m.viewport.GotoBottom()
		} else {
			m.viewport.SetYOffset(previousOffset)
		}
		return
	}

	n := len(m.messages)
	tailLive := n > 0 && m.messages[n-1].Role != "user" && m.messages[n-1].streaming
	staticCount := n
	if tailLive {
		staticCount = n - 1
	}

	var b strings.Builder
	linkRows := make(map[int]string)
	for i := 0; i < staticCount; i++ {
		if i > 0 {
			b.WriteString("\n\n")
		}
		text, rows := m.renderMessageBlock(m.messages[i])
		base := strings.Count(b.String(), "\n")
		for r, pid := range rows {
			linkRows[base+r] = pid
		}
		b.WriteString(text)
	}
	m.transcriptBody = b.String()
	m.transcriptBodyLinkRows = linkRows
	m.transcriptLiveIndicatorActive = tailLive
	m.applyTranscriptContent(followBottom, previousOffset)
}

// renderMessageBlock renders one message — a user prompt block, or the
// tool-activity/content/notices block for an assistant/tool-role message
// — in isolation. The single place this logic lives, built from by both
// refreshTranscript's static-prefix loop and applyTranscriptContent's
// tail re-render, so the two paths can never drift into rendering a
// message differently depending on which one happens to touch it.
// localLinkRows keys are row numbers relative to this block's own first
// line (0-based); the caller offsets them by however many lines came
// before the block in the full transcript.
func (m Model) renderMessageBlock(msg chatMessage) (text string, localLinkRows map[int]string) {
	var b strings.Builder
	localLinkRows = make(map[int]string)
	switch msg.Role {
	case "user":
		// User text is never run through the markdown renderer — it's
		// what was typed, not a formatted reply, and echoing it back
		// reformatted would be surprising. Right-aligned prompt block,
		// not a chat bubble — see promptblock.go and DECISIONS.md.
		b.WriteString(m.renderPromptBlock(msg.Content))
		if len(msg.Images) > 0 {
			b.WriteString("\n" + styleHint.Render(messageImagePrefix+strings.Join(msg.Images, ", ")))
		}
	default:
		for _, act := range msg.ToolActivity {
			row := strings.Count(b.String(), "\n")
			if act.ProcessID != "" {
				localLinkRows[row] = act.ProcessID
			}
			b.WriteString(m.renderToolActivity(act) + "\n")
		}
		// While a turn is live, durable notices belong above the activity
		// surface. Rendering them below it makes the K stop being the visual
		// tail of the run (the segment-notice bug caught this live). Once the
		// turn is complete, the conventional answer-then-notices order is fine.
		if msg.streaming {
			for _, n := range msg.Notices {
				b.WriteString(renderNotice(n) + "\n")
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
			// below, when the message is complete. Explicitly
			// word-wrapped to the viewport's actual current width
			// (narrower once the Ctrl+B process pane tiles it) —
			// unlike renderMarkdown's glamour output, plain
			// styleBody.Render never wrapped on its own, so a long
			// streaming line used to just get clipped by bubbles'
			// own viewport instead of wrapping.
			kramTag := styleKramTag.Render("kram") + "  "
			wrapWidth := maxInt(1, m.viewport.Width-lipgloss.Width(kramTag))
			b.WriteString(kramTag + styleBody.Width(wrapWidth).Render(msg.Content) + "\n" + m.thinkingLine())
		case msg.Content != "":
			b.WriteString(styleKramTag.Render("kram") + "  " + renderMarkdown(m.mdRenderer, msg.Content))
		}
		if !msg.streaming {
			for _, n := range msg.Notices {
				b.WriteString("\n" + renderNotice(n))
			}
			if row := renderFilesTouched(touchedFiles(msg.ToolActivity)); row != "" {
				b.WriteString("\n" + row)
			}
		}
	}
	return b.String(), localLinkRows
}

// applyTranscriptContent composes m.transcriptBody with a freshly
// re-rendered tail block (when m.transcriptLiveIndicatorActive) and sets
// the result as the viewport's content — the one place that actually
// calls SetContent for the transcript, shared by refreshTranscript's
// full rebuild and refreshLiveIndicator's cheap per-tick path.
func (m *Model) applyTranscriptContent(followBottom bool, previousOffset int) {
	content := m.transcriptBody
	linkRows := make(map[int]string, len(m.transcriptBodyLinkRows)+1)
	for row, pid := range m.transcriptBodyLinkRows {
		linkRows[row] = pid
	}
	if m.transcriptLiveIndicatorActive {
		n := len(m.messages)
		separator := ""
		if n > 1 {
			separator = "\n\n"
		}
		text, rows := m.renderMessageBlock(m.messages[n-1])
		base := strings.Count(content, "\n") + strings.Count(separator, "\n")
		for row, pid := range rows {
			linkRows[base+row] = pid
		}
		content += separator + text
	}
	if m.err != nil {
		content += "\n\n" + styleErrBadge.Render(transcriptErrPrefix+m.err.Error())
	}
	m.processLinkRows = linkRows
	m.viewport.SetContent(content)
	if followBottom {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(previousOffset)
	}
}

// refreshLiveIndicator is animTickMsg's own cheap path (see model.go and
// refreshTranscript's own doc comment): re-renders only the tail
// message's block — thinking line, and any running tool call's live
// spinner glyph, the only animFrame-dependent content that can exist —
// instead of every past message. A no-op when there's nothing live to
// animate.
func (m *Model) refreshLiveIndicator() {
	if !m.transcriptLiveIndicatorActive {
		return
	}
	followBottom := m.viewport.AtBottom()
	previousOffset := m.viewport.YOffset
	m.applyTranscriptContent(followBottom, previousOffset)
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
		meta += fmt.Sprintf(thinkingPulseFmt, m.heartbeats)
	}
	if m.segments > 1 {
		meta += fmt.Sprintf(thinkingSegmentFmt, m.segment, m.segments)
	}
	if !stalled && m.workState == workModelActive && m.reasoningPreview != "" {
		// "pensando:" keeps this unmistakably a chain-of-thought excerpt,
		// never readable as the model's actual answer — see
		// agent.EventReasoning's own doc comment for why that distinction
		// matters all the way down the stack this is fed from.
		meta += thinkingReasoningPrefix + boundedReasoningPreview(m.reasoningPreview)
	}
	// Discoverable exactly when relevant: a running turn can be interrupted
	// with Esc (see the key handler). Only shown when there's no panel to
	// close, since there Esc means "close panel" first.
	if m.active == panelNone {
		meta += thinkingInterruptHint
	}
	if stalled {
		sinceEvent := time.Since(m.lastEventAt).Round(time.Second)
		label = thinkingStalledLabel
		meta = fmt.Sprintf(thinkingStalledMetaFmt, m.stallContext(), sinceEvent, elapsed)
		return indicator + "  " + styleBadgeWarn.Bold(true).Render(label) + "  " + rail + "  " + styleBadgeWarn.Render(meta)
	}
	return indicator + "  " + shimmerText(label, m.animFrame) + "  " + rail + "  " + styleMeta.Render(meta)
}

// stallContext says what was in flight when the stream went quiet, so the
// stall warning reads as a diagnosis ("tool bash still running", "waiting
// for the model's first output") instead of the bare transport symptom —
// the daemon now heartbeats through model waits and long tool runs, so
// when this paints it's a genuine anomaly and naming the phase is the
// most useful fact the client has.
func (m Model) stallContext() string {
	switch m.workState {
	case workToolActive:
		if m.activeTool != "" {
			return fmt.Sprintf(stallCtxToolFmt, m.activeTool)
		}
		return stallCtxTool
	case workWriting:
		return stallCtxMidAnswer
	case workAnalyzingResult:
		return stallCtxAnalyzing
	default:
		return stallCtxModel
	}
}

// reasoningPreviewMaxRunes bounds the live indicator's reasoning excerpt
// to a handful of words, not the full growing chain-of-thought fragment —
// same reasoning renderToolActivity's own 60-char args truncation uses.
const reasoningPreviewMaxRunes = 50

func boundedReasoningPreview(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) > reasoningPreviewMaxRunes {
		return string(runes[:reasoningPreviewMaxRunes]) + "…"
	}
	return text
}

func (m Model) activityLabel() string {
	switch m.workState {
	case workModelActive:
		return activityModelActive
	case workToolActive:
		if m.activeTool != "" {
			return activityRunningToolPrefix + m.activeTool
		}
		return activityRunningTool
	case workAnalyzingResult:
		return activityAnalyzingResult
	case workWriting:
		return activityWriting
	default:
		return activityPreparingRoute
	}
}

func (m Model) renderActivityRail(stalled bool) string {
	const nodes = 7
	active := positiveModulo(m.animFrame/activeStepFrames, nodes)
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
		return viewStarting
	}
	if m.phase == phaseSplash {
		return m.renderBootSplash()
	}
	if m.phase == phasePicker {
		return m.renderPicker()
	}
	if m.phase == phaseAccounts {
		if m.wizardMode {
			// Step 3 of the wizard reuses the accounts screen; wrap it in
			// the shared chrome so the flow keeps one continuous identity.
			// No static key bar — the screen carries context-sensitive
			// hints of its own (connect, oauth, recheck, continue).
			return m.renderWizardFrame(3, wizardTitleProviders, m.renderAccounts(), nil, wizardWideCardMaxWidth)
		}
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
		if len(m.pendingImages) > 0 {
			b.WriteString(styleHint.Render(pendingImagesPrefix+strings.Join(imageDisplayNames(m.pendingImages), ", ")) + "\n")
		}
		b.WriteString(m.input.View())
	}
	b.WriteString("\n")

	switch m.active {
	case panelStrategy:
		b.WriteString(m.renderStrategyPanel())
	case panelStrategyPicker:
		if m.routePickerLevel == routeLevelCombo {
			b.WriteString(m.renderComboPicker())
		} else {
			b.WriteString(m.renderStrategyPicker())
		}
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
	// bgProcessBadge is deliberately first: handleMouse gives it its own
	// click zone at the left edge of this block (opens the process panel
	// directly), distinct from the rest of the block's existing
	// click-anywhere-opens-context behavior — see the badge width math
	// there.
	return joinNonEmpty("  ", m.bgProcessBadge(), m.contextIcon(), styleHint.Render(footerHintProcesses),
		styleHint.Render(footerHintRoute), styleHint.Render(footerHintContext), styleHint.Render(footerHintStrategy), styleHint.Render(footerHintDetails))
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
// generic "thinking" placeholder. Args truncation is bounded by the
// *actual current* viewport width (see toolActivityArgsLimit), not a
// bare fixed constant — a fixed cap wide enough for a full-width terminal
// still overflows and gets clipped (not wrapped) by bubbles' own
// viewport once the Ctrl+B process pane narrows the chat column, the
// concrete bug a user hit in practice.
func (m Model) renderToolActivity(act daemonclient.ToolActivity) string {
	mark := m.spin.View() // still running: real-time spinner, not a guessed outcome
	if !act.Running {
		mark = styleBadgeOK.Render("✓")
		if !act.OK {
			mark = styleBadgeBad.Render("✗")
		}
	}
	prefix := "  ↳ "
	overhead := lipgloss.Width(prefix) + lipgloss.Width(act.Name) + len("()") + 1 + lipgloss.Width(mark)
	if act.ProcessID != "" {
		overhead += lipgloss.Width(act.ProcessID) + 1
	}
	args := truncateToWidth(act.Args, toolActivityArgsLimit(m.viewport.Width, overhead))
	label := act.Name + "(" + args + ")"
	if act.ProcessID != "" {
		label = styleBadgeAccent.Render(act.ProcessID) + " " + styleMeta.Render(label)
	} else {
		label = styleMeta.Render(label)
	}
	line := styleHint.Render(prefix) + label + " " + mark
	if preview := m.renderToolResultPreview(act); preview != "" {
		line += "\n" + preview
	}
	return line
}

// toolActivityArgsLimit caps args at 60 runes on a wide-enough terminal
// (today's original tuning), narrowing to whatever actually fits
// alongside the rest of the line once viewportWidth is too small for
// that — never wider than the viewport, never narrower than 10 runes
// (a truncated-almost-to-nothing args string is still more informative
// than none at all on a very narrow pane).
func toolActivityArgsLimit(viewportWidth, overhead int) int {
	return minInt(60, maxInt(10, viewportWidth-overhead))
}

// truncateToWidth is the shared rune-bounded "…"-suffixed truncation
// renderToolActivity and renderToolResultPreview both need — width is a
// rune count, an approximation of display columns that's exact for the
// plain ASCII/Latin tool output and args this project's own tools
// produce, matching the same approximation renderFilesTouched and
// boundedReasoningPreview already make elsewhere in this package.
func truncateToWidth(s string, width int) string {
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width < 1 {
		return "…"
	}
	return string(runes[:width]) + "…"
}

// toolResultPreviewMaxLines bounds the inline preview to a handful of
// short lines — the same "a glimpse, not the whole blob" discipline
// renderNotice/boundedReasoningPreview already apply elsewhere in this
// file — not the full raw output, which can already be arbitrarily large
// (see processpanel.go's own processLocalLogMax cap on that separate,
// dedicated observer). Line *width* is bounded dynamically against the
// actual viewport (see renderToolResultPreview), not a fixed constant —
// same reasoning as toolActivityArgsLimit.
const toolResultPreviewMaxLines = 4

// toolResultPreviewIndent is the fixed left margin every preview line
// gets, subtracted from the viewport width before truncating so an
// indented line still fits within it exactly.
const toolResultPreviewIndent = "      "

// renderToolResultPreview shows a bounded excerpt of what a finished
// tool call actually printed — right under its name(args) ✓/✗ line,
// styled the same dim hint color as everything else in this file that's
// supporting detail rather than the model's own words. Only for a
// completed, non-background call: act.Running has no result yet to show
// (Kram's tool execution doesn't stream partial output mid-call — a
// real gap, not a design choice, see DECISIONS.md), and a run_background
// process already has its own dedicated live observer (Ctrl+B,
// processpanel.go) that's a strictly better place to watch its output
// than a static excerpt frozen at start time would be.
func (m Model) renderToolResultPreview(act daemonclient.ToolActivity) string {
	if act.Running || act.ProcessID != "" || strings.TrimSpace(act.Result) == "" {
		return ""
	}
	width := minInt(100, maxInt(20, m.viewport.Width-len(toolResultPreviewIndent)))
	clean := strings.ReplaceAll(ansi.Strip(act.Result), "\r\n", "\n")
	clean = strings.TrimRight(clean, "\n")
	lines := strings.Split(clean, "\n")
	shown := lines
	overflow := 0
	if len(shown) > toolResultPreviewMaxLines {
		shown = shown[:toolResultPreviewMaxLines]
		overflow = len(lines) - toolResultPreviewMaxLines
	}
	var b strings.Builder
	for i, line := range shown {
		line = truncateToWidth(strings.TrimRight(line, " \t"), width)
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(styleHint.Render(toolResultPreviewIndent + line))
	}
	if overflow > 0 {
		b.WriteString("\n" + styleHint.Render(fmt.Sprintf(toolPreviewOverflowFmt, toolResultPreviewIndent, overflow)))
	}
	return b.String()
}

// noticeWarnPhrases are the substrings of the daemon's known EventNotice
// texts (see internal/daemon/agent's EventNotice call sites in agent.go
// and retry.go) that flag a real problem — a stagnating retry loop, a
// transient gateway failure the daemon is retrying around, or the daemon
// having to actively stop a provider from leaking raw tool markup — as
// opposed to a benign informational notice (compaction ran, an image
// capability fallback, markup the daemon simply normalized and moved
// past). This is matched against a fixed, known set of daemon-emitted
// strings, not a general-purpose classifier.
var noticeWarnPhrases = []string{
	"stagnation detected",
	"transient gateway failure",
	"Kram stopped it",
}

func noticeIsWarning(text string) bool {
	for _, phrase := range noticeWarnPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// renderNotice is the single place a daemon notice's free text becomes a
// transcript line — extending renderToolActivity's own terse-glyph
// discipline to notices, which previously all rendered as an identical
// hint-styled bullet regardless of whether they reported something worth
// a second look or something entirely routine.
func renderNotice(text string) string {
	glyph := styleHint.Render("· ")
	if noticeIsWarning(text) {
		glyph = styleBadgeWarn.Render("⚠ ")
	}
	return glyph + styleHint.Render(text)
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
