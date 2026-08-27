package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/statusclient"
)

// availableCombos is the set of combos the gateway reports — the rows of the
// first (combo) level of the Ctrl+S routing panel.
func (m Model) availableCombos() []statusclient.Combo {
	return m.strategyData.Combos
}

// syncComboPickerFocus lands the highlight on the combo the daemon is
// currently routing through, so opening the panel starts on the active combo
// rather than always at the top.
func (m *Model) syncComboPickerFocus() {
	combos := m.availableCombos()
	for i := range combos {
		if combos[i].ID == m.combo {
			m.comboPickerFocus = i
			return
		}
	}
	if n := len(combos); n == 0 {
		m.comboPickerFocus = 0
	} else if m.comboPickerFocus >= n {
		m.comboPickerFocus = n - 1
	}
}

// comboPickerWindow mirrors strategyPickerWindow: it keeps the focused combo
// roughly centered within the panel's height budget so a long combo list
// scrolls instead of overflowing.
func (m Model) comboPickerWindow() (start, end int) {
	n := len(m.availableCombos())
	visible := m.panelHeight() - 4 // title, spacer, optional single-provider note, hint
	if visible < 1 {
		visible = 1
	}
	if visible > n {
		visible = n
	}
	start = m.comboPickerFocus - visible/2
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

// comboProviderSummary renders the parenthetical after a combo's name: how
// many providers it fans across and its current strategy — the two facts that
// decide whether picking it leads anywhere (a single-provider combo has no
// routing to configure).
func comboProviderSummary(c statusclient.Combo) string {
	n := len(c.Providers)
	unit := comboProvidersPlural
	if n == 1 {
		unit = comboProvidersSingular
	}
	return fmt.Sprintf("%d %s · %s", n, unit, normalizeStrategy(c.Strategy))
}

func (m Model) renderComboPicker() string {
	h := m.panelHeight()
	lines := []string{styleMeta.Render(comboPickerTitle), ""}

	combos := m.availableCombos()
	if len(combos) == 0 {
		message := comboPickerLoading
		if m.strategyErr != nil {
			message = strategyPickerQueryErrPrefix + m.strategyErr.Error()
		}
		lines = append(lines, styleHint.Render(message))
		return padLines(lines, h, m.width)
	}

	start, end := m.comboPickerWindow()
	for i := start; i < end; i++ {
		c := combos[i]
		marker := "  "
		style := styleBody
		if i == m.comboPickerFocus {
			marker = "▸ "
			style = styleBadgeAccent
		}
		active := ""
		if c.ID == m.combo {
			active = "  " + styleBadgeOK.Render(comboPickerActiveBadge)
		}
		lines = append(lines, marker+style.Render(c.ID)+"  "+styleMeta.Render(comboProviderSummary(c))+active)
	}

	switch {
	case m.strategyPickerErr != nil:
		lines = append(lines, "", styleErrBadge.Render(strategyPickerFailPrefix+m.strategyPickerErr.Error()))
	case len(combos[clampInt(m.comboPickerFocus, 0, len(combos)-1)].Providers) < 2:
		// The highlighted combo fans across a single provider, so selecting
		// it leads nowhere to configure — say so up front rather than after
		// the pick.
		lines = append(lines, "", styleMeta.Render(comboPickerSingleProvider), styleHint.Render(comboPickerHint))
	default:
		lines = append(lines, "", styleHint.Render(comboPickerHint))
	}
	return padLines(lines, h, m.width)
}

// selectFocusedCombo switches the daemon's active combo to the highlighted
// one. A combo that fans across two or more providers has a strategy worth
// configuring, so the panel advances to the strategy level; a single-provider
// combo does not, so the panel just confirms the switch and closes.
func (m Model) selectFocusedCombo() (tea.Model, tea.Cmd) {
	combos := m.availableCombos()
	if len(combos) == 0 {
		return m, nil
	}
	idx := clampInt(m.comboPickerFocus, 0, len(combos)-1)
	selected := combos[idx]

	// Optimistically point the local view at the new combo so the strategy
	// level (which reads currentCombo via m.combo) shows the right combo
	// immediately; the daemon switch is fired alongside and confirmed async.
	m.combo = selected.ID
	m.comboPickerFocus = idx
	switchCmd := setComboCmd(m.daemon, selected.ID)

	if len(selected.Providers) >= 2 {
		m.routePickerLevel = routeLevelStrategy
		m.strategyPickerErr = nil
		// Drop the previous combo's last route_done so the strategy level's
		// focus falls back to *this* combo's current strategy, not a stale
		// label from a call the old combo made.
		m.routeCall = nil
		m.syncStrategyPickerFocus()
		return m, switchCmd
	}

	// Single provider: nothing to route. Confirm the switch and close.
	m.strategyNoticeRev++
	m.strategyNotice = comboSwitchedPrefix + selected.ID + comboSingleNoticeSuffix
	m.active = panelNone
	m.routePickerLevel = routeLevelCombo
	m.syncViewportSize()
	m.syncTranscriptRenderer()
	return m, tea.Batch(switchCmd, clearStrategyNoticeCmd(m.strategyNoticeRev))
}

// comboPickerIndexAtRow maps an absolute terminal row to the visible combo
// list, mirroring strategyPickerIndexAtRow: the panel begins after the route
// bar, transcript and composer, and its first two rows are title and spacer.
func (m Model) comboPickerIndexAtRow(y int) (int, bool) {
	panelStart := routeBarHeight + m.viewport.Height + inputHeight
	relative := y - panelStart
	if relative < 2 {
		return 0, false
	}
	start, end := m.comboPickerWindow()
	index := start + relative - 2
	return index, index >= start && index < end
}
