package app

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/truncate"

	"github.com/codexmark/kram/internal/openai"
)

// Width tiers the route bar degrades across, narrowest first in reading
// order below — see DECISIONS.md, "Route bar responsiveness."
const (
	routeBarWideMin   = 78
	routeBarMediumMin = 46
)

// renderRouteBar draws the discreet top status strip: the active
// strategy name plus, once known, the real fallback trail for the
// current turn's most recent model call. Nothing here is simulated —
// outside of a generic "routing…" pulse while a call is actually in
// flight (there is no per-attempt progress to show mid-call; the
// gateway's own fallback loop happens inside one HTTP round-trip the
// daemon only ever sees the result of, see DECISIONS.md), every glyph and
// number comes from a real route_done/done event.
func (m Model) renderRouteBar() string {
	strategy := m.routeBarStrategyLabel()
	if strategy == "" {
		return "" // status hasn't loaded yet — stay blank rather than guess
	}

	var trail string
	switch {
	case m.routeRunning:
		trail = styleBadgeWarn.Render("◉ roteando…")
	case m.routeCall != nil && len(m.routeCall.Attempts) > 0:
		trail = m.renderRouteAttempts(m.routeCall.Attempts)
	}

	left := joinNonEmpty("   ", styleBadgeAccent.Render(strings.ToUpper(strategy)), trail)

	right := ""
	if m.routeCall != nil && len(m.routeCall.Attempts) > 0 {
		right = styleHint.Render(fmt.Sprintf("call %d", m.routeCall.Index))
	}
	// padBetween already drops the right block when nothing fits, but a
	// long enough left side (real provider IDs have no length limit) can
	// still overflow on its own — this is the final safety net so the
	// route bar never wraps to a second line or bleeds past the
	// terminal's edge, ANSI-color-code aware so a truncated line doesn't
	// leave a dangling escape sequence. Only actually truncates when the
	// line really is too long: reflow's truncate unconditionally reserves
	// room for the tail, so calling it on a line already exactly at width
	// would clip its last character and add "…" for no reason.
	result := padBetween(m.width, left, right)
	if m.width > 0 && lipgloss.Width(result) > m.width {
		return truncate.StringWithTail(result, uint(m.width), "…")
	}
	return result
}

// routeBarStrategyLabel prefers the strategy the most recent real
// route_done reported (authoritative, from the router itself) and falls
// back to the gateway's /admin/status combo listing (prefetched on
// entering chat — see enterChatCmds) for the label before any turn has
// run yet.
func (m Model) routeBarStrategyLabel() string {
	if m.routeCall != nil && m.routeCall.Strategy != "" {
		return m.routeCall.Strategy
	}
	if combo := m.currentCombo(); combo != nil {
		if combo.Strategy == "" {
			return "priority" // v0's "" strategy string means declared order
		}
		return combo.Strategy
	}
	return ""
}

// renderRouteAttempts draws the fallback trail, degrading by terminal
// width: full "provider glyph latency" segments when wide, glyph+name
// without individual latencies at medium width, bare glyphs when narrow.
func (m Model) renderRouteAttempts(attempts []openai.AttemptInfo) string {
	switch {
	case m.width >= routeBarWideMin:
		return renderRouteAttemptsWide(attempts)
	case m.width >= routeBarMediumMin:
		return renderRouteAttemptsMedium(attempts)
	default:
		return renderRouteAttemptsNarrow(attempts)
	}
}

func renderRouteAttemptsWide(attempts []openai.AttemptInfo) string {
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		seg := fmt.Sprintf("%s %s %s", a.Provider, outcomeGlyph(a.Outcome), formatLatency(a.LatencyMS))
		parts = append(parts, outcomeStyle(a.Outcome).Render(seg))
	}
	return strings.Join(parts, styleHint.Render(" ── "))
}

func renderRouteAttemptsMedium(attempts []openai.AttemptInfo) string {
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		parts = append(parts, outcomeStyle(a.Outcome).Render(a.Provider+" "+outcomeGlyph(a.Outcome)))
	}
	trail := strings.Join(parts, styleHint.Render(" ── "))
	word := "tentativa"
	if len(attempts) != 1 {
		word = "tentativas"
	}
	return trail + "   " + styleHint.Render(fmt.Sprintf("%d %s", len(attempts), word))
}

func renderRouteAttemptsNarrow(attempts []openai.AttemptInfo) string {
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		parts = append(parts, outcomeStyle(a.Outcome).Render("●"+outcomeGlyph(a.Outcome)))
	}
	return strings.Join(parts, styleHint.Render("─"))
}

// outcomeGlyph never depends on color alone to carry meaning — a
// distinct glyph per outcome, checked before this is ever rendered onto
// a badge style, so the route bar stays legible without color (see
// DECISIONS.md, "route bar accessibility").
func outcomeGlyph(o openai.AttemptOutcome) string {
	switch o {
	case openai.OutcomeSuccess:
		return "✓"
	case openai.OutcomeError:
		return "✕"
	case openai.OutcomeRejected:
		return "!"
	case openai.OutcomeSkipped:
		return "·"
	default:
		return "◉"
	}
}

func outcomeStyle(o openai.AttemptOutcome) lipgloss.Style {
	switch o {
	case openai.OutcomeSuccess:
		return styleBadgeOK
	case openai.OutcomeError, openai.OutcomeRejected:
		return styleBadgeBad
	default:
		return styleBadgeWarn
	}
}

func formatLatency(ms int64) string {
	if ms >= 1000 {
		return strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64) + "s"
	}
	return strconv.FormatInt(ms, 10) + "ms"
}
