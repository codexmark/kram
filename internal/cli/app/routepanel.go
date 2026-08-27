package app

import (
	"fmt"
	"strings"

	"github.com/codexmark/kram/internal/openai"
)

// renderRoutePanel draws the expanded Ctrl+R route trace: every model
// call in the most recently completed turn, every attempt within each
// (success, error, or gate-rejected — never conflated), and a summary of
// the whole turn's routing activity. Every number here comes straight
// from the daemon's RouteTrace (see agent.RouteTrace, DECISIONS.md) —
// nothing is recomputed or guessed, and this is the fix for a real
// former limitation: only the *last* model call's fallback trail used to
// be visible at all, so a multi-tool-call turn's earlier routing
// decisions were invisible even though they really happened.
func (m Model) renderRoutePanel() string {
	h := m.panelHeight()
	var lines []string

	if len(m.routeTrace.Calls) == 0 {
		lines = append(lines, styleMeta.Render(routeNoCalls))
		return padLines(lines, h, m.width)
	}

	lines = append(lines, styleMeta.Render("ROUTE TRACE · "+displayStrategy(m.routeTrace.Strategy)))
	lines = append(lines, "")

	totalAttempts := 0
	fallbacks := 0
	providers := map[string]bool{}
	var providerTimeMS int64

	for _, call := range m.routeTrace.Calls {
		rankingByID := make(map[string]openai.RankedProviderInfo, len(call.Ranking))
		for _, r := range call.Ranking {
			rankingByID[r.Provider] = r
		}
		if len(call.Attempts) > 1 {
			fallbacks++
		}

		lines = append(lines, styleBadgeAccent.Render(fmt.Sprintf("#%d", call.Index)))
		for _, a := range call.Attempts {
			totalAttempts++
			providers[a.Provider] = true
			providerTimeMS += a.LatencyMS

			mark := outcomeStyle(a.Outcome).Render(outcomeGlyph(a.Outcome))
			lines = append(lines, fmt.Sprintf("  %-22s %8s  %s", a.Provider, formatLatency(a.LatencyMS), mark))
			if detail := routeAttemptDetail(a, rankingByID[a.Provider]); detail != "" {
				lines = append(lines, styleHint.Render("      "+detail))
			}
		}
		lines = append(lines, "")
	}

	ruleWidth := m.width
	if ruleWidth > 50 {
		ruleWidth = 50
	}
	lines = append(lines, styleHint.Render(strings.Repeat("─", ruleWidth)))
	lines = append(lines, styleMeta.Render(fmt.Sprintf(routeModelCallsLine, len(m.routeTrace.Calls), pluralPT(len(m.routeTrace.Calls), routeCallSingular, routeCallPlural))))
	lines = append(lines, styleMeta.Render(fmt.Sprintf(routeUpstreamLine, totalAttempts, pluralPT(totalAttempts, routeAttemptSingular, routeAttemptPlural))))
	lines = append(lines, styleMeta.Render(fmt.Sprintf("%d fallback%s", fallbacks, pluralSuffix(fallbacks))))
	lines = append(lines, styleMeta.Render(fmt.Sprintf("%d provider%s", len(providers), pluralSuffix(len(providers)))))
	lines = append(lines, styleMeta.Render(formatLatency(providerTimeMS)+routeProviderTimeSuffix))

	return padLines(lines, h, m.width)
}

// routeAttemptDetail explains one attempt's line: for a success, its
// score and any reasons the strategy tagged it with (sticky, last-known-
// good, cache-affinity, explore — see router.RankedCandidate); for a
// failure, the real reason text the router/gate/executor recorded.
func routeAttemptDetail(a openai.AttemptInfo, ranking openai.RankedProviderInfo) string {
	switch a.Outcome {
	case openai.OutcomeSuccess:
		parts := []string{routeAttemptSelected}
		if a.Score != nil {
			parts = append(parts, fmt.Sprintf("score %.2f", *a.Score))
		}
		for _, r := range ranking.Reasons {
			parts = append(parts, strings.ToUpper(r))
		}
		return strings.Join(parts, " · ")
	case openai.OutcomeError, openai.OutcomeRejected:
		if a.Reason != "" {
			return a.Reason
		}
		return string(a.Outcome)
	default:
		return ""
	}
}

func displayStrategy(s string) string {
	if s == "" {
		return "priority"
	}
	return s
}

func pluralPT(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
