package app

import (
	"fmt"
	"strings"

	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
)

// renderPanel draws the end-to-end strategy panel: every provider in the
// active combo's fallback chain, staggered to show routing order, each
// tagged with its live circuit-breaker state and real telemetry from the
// gateway. Everything shown here comes straight from GET /admin/status —
// nothing is inferred or simulated.
func (m Model) renderPanel() string {
	h := m.panelHeight()
	var lines []string

	if m.panelErr != nil {
		lines = append(lines, styleErrBadge.Render("não consegui falar com o gateway: "+m.panelErr.Error()))
		return padLines(lines, h, m.width)
	}

	combo := m.currentCombo()
	if combo == nil {
		lines = append(lines, styleMeta.Render("nenhum combo configurado no gateway"))
		return padLines(lines, h, m.width)
	}

	statsByID := make(map[string]statusclient.Provider, len(m.panelData.Providers))
	for _, p := range m.panelData.Providers {
		statsByID[p.ID] = p
	}

	lines = append(lines, styleMeta.Render(fmt.Sprintf("combo %s · estratégia %s", combo.ID, combo.Strategy)))
	lines = append(lines, "")

	focus := m.panelFocus
	if focus >= len(combo.Providers) {
		focus = len(combo.Providers) - 1
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
