package app

import (
	"fmt"
	"strings"

	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
	"github.com/codexmark/kram-gateway/internal/openai"
)

// renderStrategyPanel draws the end-to-end strategy panel: every provider
// in the active combo's fallback chain, staggered to show routing order,
// each tagged with its live circuit-breaker state and real telemetry from
// the gateway. Everything shown here comes straight from GET
// /admin/status — nothing is inferred or simulated.
func (m Model) renderStrategyPanel() string {
	h := m.panelHeight()
	var lines []string

	if m.strategyErr != nil {
		lines = append(lines, styleErrBadge.Render("não consegui falar com o gateway: "+m.strategyErr.Error()))
		return padLines(lines, h, m.width)
	}

	combo := m.currentCombo()
	if combo == nil {
		lines = append(lines, styleMeta.Render("nenhum combo configurado no gateway"))
		return padLines(lines, h, m.width)
	}

	statsByID := make(map[string]statusclient.Provider, len(m.strategyData.Providers))
	for _, p := range m.strategyData.Providers {
		statsByID[p.ID] = p
	}

	lines = append(lines, styleMeta.Render(fmt.Sprintf("combo %s · estratégia %s", combo.ID, combo.Strategy)))
	lines = append(lines, "")

	focus := m.strategyFocus
	if focus >= len(combo.Providers) {
		focus = len(combo.Providers) - 1
	}

	// Real scoring data for the focused provider (the most recent turn's
	// actual ranking — see agent.RouteTrace via m.routeCall) replaces the
	// plain badge/telemetry view with a focused breakdown, matching
	// section 19's shape: this provider's factors, then a compact list of
	// every other candidate's score for context. Never recomputed here —
	// the TUI only ever renders what the router already decided (see
	// DECISIONS.md). Non-scoring strategies (priority, round-robin,
	// prefix-affinity) leave Ranking empty, so the plain view below is
	// what those always show.
	if focus >= 0 && focus < len(combo.Providers) {
		if info, ok := m.focusedRanking(combo.Providers[focus]); ok {
			lines = append(lines, renderScoreBreakdown(info)...)
			lines = append(lines, "")
			lines = append(lines, otherScores(m.routeCall.Ranking, info.Provider)...)
			lines = append(lines, "")
			lines = append(lines, styleHint.Render("↑↓ trocar candidato"))
			return padLines(lines, h, m.width)
		}
	}

	for i, pid := range combo.Providers {
		indent := strings.Repeat("  ", i)
		connector := ""
		if i > 0 {
			connector = "└──"
		}

		name := pid
		if i == focus {
			name = "▸ " + name
		} else {
			name = "  " + name
		}

		badge := badgeForProvider(statsByID[pid], pid)
		lines = append(lines, indent+styleHint.Render(connector)+" "+styleBody.Render(name)+"  "+badge)
	}

	lines = append(lines, "")
	lines = append(lines, styleMeta.Render(explainProvider(combo, statsByID, focus)))

	return padLines(lines, h, m.width)
}

// otherScores lists every other ranked candidate's score, compactly —
// the "gemini .842, openai .816" context section 19 asks for, so a
// candidate that was never even attempted (fallback stopped before
// reaching it) is still visible.
func otherScores(ranking []openai.RankedProviderInfo, exclude string) []string {
	var lines []string
	for _, r := range ranking {
		if r.Provider == exclude {
			continue
		}
		lines = append(lines, styleMeta.Render(fmt.Sprintf("%-20s %.3f", r.Provider, r.Score)))
	}
	return lines
}

// focusedRanking looks up providerID's entry in the most recently
// completed model call's full ranking, if any scoring strategy produced
// one.
func (m Model) focusedRanking(providerID string) (openai.RankedProviderInfo, bool) {
	if m.routeCall == nil {
		return openai.RankedProviderInfo{}, false
	}
	for _, r := range m.routeCall.Ranking {
		if r.Provider == providerID {
			return r, true
		}
	}
	return openai.RankedProviderInfo{}, false
}

// renderScoreBreakdown formats one candidate's factor-by-factor score —
// weight × value = contribution, per factor — plus any reasons (sticky,
// last-known-good, cache-affinity, explore) tagged on top of the base
// score. Deliberately compact (one line per factor, reasons combined onto
// a single line): the panel's height is shared with every other section
// above it, and this is real content, not a full-screen view.
func renderScoreBreakdown(info openai.RankedProviderInfo) []string {
	lines := []string{
		styleBadgeAccent.Render(info.Provider) + "  " + styleMeta.Render(fmt.Sprintf("score %.3f", info.Score)),
	}
	longest := 0
	for _, f := range info.Factors {
		if len(f.Name) > longest {
			longest = len(f.Name)
		}
	}
	for _, f := range info.Factors {
		lines = append(lines, styleMeta.Render(fmt.Sprintf(
			"%-*s %5.0f%% x %.2f = %.3f", longest, f.Name, f.Value*100, f.Weight, f.Contribution,
		)))
	}
	if len(info.Reasons) > 0 {
		upper := make([]string, len(info.Reasons))
		for i, r := range info.Reasons {
			upper[i] = strings.ToUpper(r)
		}
		lines = append(lines, styleBadgeOK.Render(strings.Join(upper, " · ")))
	}
	return lines
}

func badgeForProvider(p statusclient.Provider, id string) string {
	if p.ID == "" {
		return styleBadgeIdle.Render("sem dados ainda")
	}
	state := "closed"
	style := styleBadgeOK
	if p.BreakerOpen {
		state = "open"
		style = styleBadgeBad
	} else if p.Stats.Requests > 0 && p.Stats.SuccessRate < 1 {
		state = "instável"
		style = styleBadgeWarn
	}

	parts := []string{style.Render(state)}
	if p.Stats.AvgLatencyMS > 0 {
		parts = append(parts, styleMeta.Render(fmt.Sprintf("%dms", p.Stats.AvgLatencyMS)))
	}
	if p.Stats.Requests > 0 {
		parts = append(parts, styleMeta.Render(fmt.Sprintf("%.0f%% sucesso", p.Stats.SuccessRate*100)))
	}
	return strings.Join(parts, " · ")
}

func explainProvider(combo *statusclient.Combo, statsByID map[string]statusclient.Provider, focus int) string {
	if focus < 0 || focus >= len(combo.Providers) {
		return ""
	}
	pid := combo.Providers[focus]
	p := statsByID[pid]

	position := "entra na rotação"
	if focus == 0 {
		position = "primeiro da ordem de fallback"
	} else {
		position = fmt.Sprintf("assume se os %d anterior(es) falharem ou estiverem com circuito aberto", focus)
	}

	if p.BreakerOpen {
		return fmt.Sprintf("▸ %s: circuito aberto agora — está sendo pulado até o próximo teste automático. %s.", pid, position)
	}
	if p.Stats.Requests == 0 {
		return fmt.Sprintf("▸ %s: %s. Ainda sem requisições nesta sessão do gateway.", pid, position)
	}
	return fmt.Sprintf("▸ %s: %s. %d requisições, %.0f%% de sucesso, %dms de latência média.",
		pid, position, p.Stats.Requests, p.Stats.SuccessRate*100, p.Stats.AvgLatencyMS)
}

func padLines(lines []string, height, width int) string {
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = stylePanelBG.Width(width).Render(l)
	}
	return strings.Join(out, "\n") + "\n"
}
