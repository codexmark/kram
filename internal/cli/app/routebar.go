package app

import (
	"fmt"
	"math"
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
// current turn's most recent model call. While a call is in flight, an
// animated rail uses the real number of configured candidates without
// pretending it knows which individual provider the gateway is currently
// trying (that arrives only with route_done; see DECISIONS.md).
func (m Model) renderRouteBar() string {
	strategy := m.routeBarStrategyLabel()
	if strategy == "" {
		return "" // status hasn't loaded yet — stay blank rather than guess
	}

	var trail string
	switch {
	case m.routeRunning:
		trail = m.renderRoutingActivity()
	case m.routeCall != nil && len(m.routeCall.Attempts) > 0:
		trail = m.renderRouteAttempts(m.routeCall.Attempts)
	}

	strategyBlock := styleHint.Render(routeStrategyPrefix) + " " + styleBadgeAccent.Render(strings.ToUpper(strategy)) + styleHint.Render(" ▾")
	left := joinNonEmpty("   ", strategyBlock, trail)

	right := ""
	if index := m.routeBarCallIndex(); index > 0 {
		right = styleHint.Render(fmt.Sprintf("call %d", index))
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

// routeBarStrategyWidth is the exact clickable width of the strategy block at
// the left edge; the attempt trail beside it remains a passive diagnostic.
func (m Model) routeBarStrategyWidth() int {
	strategy := m.routeBarStrategyLabel()
	if strategy == "" {
		return 0
	}
	block := styleHint.Render(routeStrategyPrefix) + " " + styleBadgeAccent.Render(strings.ToUpper(strategy)) + styleHint.Render(" ▾")
	return lipgloss.Width(block)
}

// renderRoutingActivity is intentionally a candidate rail, not a fake live
// attempt trace. route_start tells the CLI that routing is active and the
// status snapshot tells it how many candidates exist; the provider identity
// and outcome of each real attempt arrive together in route_done and replace
// this rail immediately.
func (m Model) renderRoutingActivity() string {
	candidates := m.routeCandidateCount()
	nodes := candidates
	if nodes == 0 {
		nodes = 3 // generic motion when status has not loaded; no count is claimed
	}
	if nodes > 5 {
		nodes = 5
	}

	active := positiveModulo(m.animFrame/activeStepFrames, nodes)
	parts := make([]string, nodes)
	for i := range parts {
		if i != active {
			parts[i] = styleHint.Render("○")
			continue
		}
		phase := float64(m.animFrame)*shimmerPhasePerFrame + float64(i)*math.Pi/2
		blend := (math.Sin(phase) + 1) / 2
		color := shimmerFrom.BlendLuv(shimmerTo, blend)
		parts[i] = lipgloss.NewStyle().Foreground(lipgloss.Color(color.Hex())).Bold(true).Render("◉")
	}
	rail := strings.Join(parts, styleFaintTrack.Render("─"))

	switch {
	case m.width >= routeBarWideMin && candidates > 0:
		return rail + " " + styleMeta.Render(fmt.Sprintf(routeEvaluatingFmt, candidates))
	case m.width >= routeBarMediumMin && candidates > 0:
		return rail + " " + styleMeta.Render(fmt.Sprintf(routeRoutesFmt, candidates))
	default:
		return rail
	}
}

func (m Model) routeCandidateCount() int {
	if combo := m.currentCombo(); combo != nil {
		return len(combo.Providers)
	}
	if m.routeCall != nil {
		if len(m.routeCall.Ranking) > 0 {
			return len(m.routeCall.Ranking)
		}
		return len(m.routeCall.Attempts)
	}
	return 0
}

func (m Model) routeBarCallIndex() int {
	if m.routeRunning {
		if m.routeCall != nil {
			return m.routeCall.Index + 1
		}
		return 1
	}
	if m.routeCall != nil && len(m.routeCall.Attempts) > 0 {
		return m.routeCall.Index
	}
	return 0
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
	word := routeAttemptSingular
	if len(attempts) != 1 {
		word = routeAttemptsPlural
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
