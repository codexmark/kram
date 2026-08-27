package app

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var strategyDescriptions = map[string]string{
	"priority":        strategyDescPriority,
	"round-robin":     strategyDescRoundRobin,
	"prefix-affinity": strategyDescPrefixAffinity,
	"smart":           strategyDescSmart,
	"quality":         strategyDescQuality,
	"fast":            strategyDescFast,
	"cheap":           strategyDescCheap,
	"reliable":        strategyDescReliable,
	"weighted":        strategyDescWeighted,
	"lkgp":            strategyDescLKGP,
	"p2c":             strategyDescP2C,
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
	lines := []string{styleMeta.Render(strategyPickerTitlePrefix + comboID), ""}

	strategies := m.availableStrategies()
	if len(strategies) == 0 {
		message := strategyPickerLoading
		if m.strategyErr != nil {
			message = strategyPickerQueryErrPrefix + m.strategyErr.Error()
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
			active = "  " + styleBadgeOK.Render(strategyPickerActiveBadge)
		}
		lines = append(lines, marker+style.Render(strings.ToUpper(name))+active)
	}

	selected := strategies[clampInt(m.strategyPickerFocus, 0, len(strategies)-1)]
	description := strategyDescriptions[selected]
	if description == "" {
		description = strategyDescFallback
	}
	lines = append(lines, "", styleMeta.Render(description))
	switch {
	case m.strategyPickerErr != nil:
		lines = append(lines, styleErrBadge.Render(strategyPickerFailPrefix+m.strategyPickerErr.Error()))
	case m.strategySaving:
		lines = append(lines, styleBadgeWarn.Render(strategyPickerSaving))
	case m.strategySwitching:
		lines = append(lines, styleBadgeWarn.Render(strategyPickerApplying))
	default:
		lines = append(lines, styleHint.Render(strategyPickerHint))
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
		m.strategyPickerErr = fmt.Errorf(strategyPickerNoComboErr)
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
	return m, setStrategyCmd(m.gateway, combo.ID, selected, false, false)
}

// saveFocusedStrategy applies the highlighted strategy and persists it to the
// gateway's config.yaml, making this combo the boot default — the "save"
// action of the strategy level (Ctrl+S). Unlike applyFocusedStrategy it always
// sends even when the strategy is unchanged, because make_default is a
// meaningful write on its own (the combo may not be the default yet).
func (m Model) saveFocusedStrategy() (tea.Model, tea.Cmd) {
	if m.strategySwitching {
		return m, nil
	}
	strategies := m.availableStrategies()
	combo := m.currentCombo()
	if combo == nil || len(strategies) == 0 {
		m.strategyPickerErr = fmt.Errorf(strategyPickerNoComboErr)
		return m, nil
	}
	index := clampInt(m.strategyPickerFocus, 0, len(strategies)-1)
	selected := strategies[index]
	m.strategySwitching = true
	m.strategySaving = true
	m.strategyPickerErr = nil
	return m, setStrategyCmd(m.gateway, combo.ID, selected, true, true)
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
