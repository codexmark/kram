package app

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var strategyDescriptions = map[string]string{
	"priority":        "ordem declarada; usa o primeiro provider saudável",
	"round-robin":     "alterna o primeiro provider entre chamadas",
	"prefix-affinity": "mantém prefixos semelhantes no mesmo provider",
	"smart":           "equilibra saúde, qualidade, latência e afinidade",
	"quality":         "prioriza a estimativa de qualidade",
	"fast":            "prioriza menor latência observada",
	"cheap":           "prioriza menor custo configurado",
	"reliable":        "prioriza histórico de sucesso",
	"weighted":        "usa os pesos personalizados do combo",
	"lkgp":            "prefere o último provider que respondeu bem",
	"p2c":             "compara dois candidatos e escolhe o mais saudável",
}

func (m Model) availableStrategies() []string {
	return m.strategyData.Strategies
}

func (m *Model) syncStrategyPickerFocus() {
	current := m.routeBarStrategyLabel()
	if current == "" {
		current = "priority"
	}
	for i, name := range m.availableStrategies() {
		if name == current {
			m.strategyPickerFocus = i
			return
		}
	}
	if n := len(m.availableStrategies()); n == 0 {
		m.strategyPickerFocus = 0
	} else if m.strategyPickerFocus >= n {
		m.strategyPickerFocus = n - 1
	}
}

func (m Model) strategyPickerWindow() (start, end int) {
	n := len(m.availableStrategies())
	visible := m.panelHeight() - 5 // title, spacer, description, spacer, hint
	if visible < 1 {
		visible = 1
	}
	if visible > n {
		visible = n
	}
	start = m.strategyPickerFocus - visible/2
	if start < 0 {
		start = 0
	}
	if start+visible > n {
		start = n - visible
	}
	if start < 0 {
		start = 0
	}
	return start, start + visible
}

func (m Model) renderStrategyPicker() string {
	h := m.panelHeight()
	combo := m.currentCombo()
	comboID := m.combo
	if combo != nil {
		comboID = combo.ID
	}
	lines := []string{styleMeta.Render("trocar estratégia · combo " + comboID), ""}

	strategies := m.availableStrategies()
	if len(strategies) == 0 {
		message := "carregando estratégias do gateway…"
		if m.strategyErr != nil {
			message = "não consegui consultar o gateway: " + m.strategyErr.Error()
		}
		lines = append(lines, styleHint.Render(message))
		return padLines(lines, h, m.width)
	}

	start, end := m.strategyPickerWindow()
	for i := start; i < end; i++ {
		name := strategies[i]
		marker := "  "
		style := styleBody
		if i == m.strategyPickerFocus {
			marker = "▸ "
			style = styleBadgeAccent
		}
		active := ""
		if combo != nil && normalizeStrategy(combo.Strategy) == name {
			active = "  " + styleBadgeOK.Render("ATIVA")
		}
		lines = append(lines, marker+style.Render(strings.ToUpper(name))+active)
	}

	selected := strategies[clampInt(m.strategyPickerFocus, 0, len(strategies)-1)]
	description := strategyDescriptions[selected]
	if description == "" {
		description = "estratégia disponibilizada pelo gateway"
	}
	lines = append(lines, "", styleMeta.Render(description))
	if m.strategyPickerErr != nil {
		lines = append(lines, styleErrBadge.Render("falha: "+m.strategyPickerErr.Error()))
	} else if m.strategySwitching {
		lines = append(lines, styleBadgeWarn.Render("aplicando na próxima chamada…"))
	} else {
		lines = append(lines, styleHint.Render("↑↓ escolher · enter aplicar · esc cancelar · clique aplica"))
	}
	return padLines(lines, h, m.width)
}

func normalizeStrategy(name string) string {
	if name == "" {
		return "priority"
	}
	return name
}

func (m Model) applyFocusedStrategy() (tea.Model, tea.Cmd) {
	if m.strategySwitching {
		return m, nil
	}
	strategies := m.availableStrategies()
	combo := m.currentCombo()
	if combo == nil || len(strategies) == 0 {
		m.strategyPickerErr = fmt.Errorf("gateway ainda não informou combo e estratégias")
		return m, nil
	}
	index := clampInt(m.strategyPickerFocus, 0, len(strategies)-1)
	selected := strategies[index]
	if normalizeStrategy(combo.Strategy) == selected {
		m.active = panelNone
		m.syncViewportSize()
		m.syncTranscriptRenderer()
		return m, nil
	}
	m.strategySwitching = true
	m.strategyPickerErr = nil
	return m, setStrategyCmd(m.gateway, combo.ID, selected)
}

// strategyPickerIndexAtRow maps an absolute terminal row to the visible
// strategy list. The picker begins after the one-line route bar, transcript,
// and fixed-height composer; its first two rows are title and spacer.
func (m Model) strategyPickerIndexAtRow(y int) (int, bool) {
	panelStart := routeBarHeight + m.viewport.Height + inputHeight
	relative := y - panelStart
	if relative < 2 {
		return 0, false
	}
	start, end := m.strategyPickerWindow()
	index := start + relative - 2
	return index, index >= start && index < end
}

type strategyNoticeClearMsg struct{ revision int }

func clearStrategyNoticeCmd(revision int) tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return strategyNoticeClearMsg{revision: revision}
	})
}
