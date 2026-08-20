package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/toolsettings"
)

// toolToggleItem is one row on the tools/skills screen — a built-in tool
// or a discovered skill, whichever the daemon reported. Name/description
// come from the daemon (it's the only thing that actually knows what's
// registered); on/off state comes from the CLI's own local
// toolsettings.Store. Every mutation is also reconciled into the live daemon
// so the current and subsequent sessions observe the same effective state.
type toolToggleItem struct {
	name        string
	description string
	kind        string // "tool" or "skill"
}

func (m *Model) toolToggleItems() []toolToggleItem {
	items := make([]toolToggleItem, 0, len(m.toolsList)+len(m.skillsList))
	for _, t := range m.toolsList {
		items = append(items, toolToggleItem{name: t.Name, description: t.Description, kind: "tool"})
	}
	for _, s := range m.skillsList {
		items = append(items, toolToggleItem{name: s.Name, description: s.Description, kind: "skill"})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].kind != items[j].kind {
			return items[i].kind < items[j].kind // tools before skills
		}
		return items[i].name < items[j].name
	})
	return items
}

func (m Model) renderToolsToggle() string {
	var b strings.Builder
	b.WriteString(styleBody.Render("ferramentas e skills") + "\n\n")

	if m.toolsErr != nil {
		b.WriteString(styleErrBadge.Render("erro: "+m.toolsErr.Error()) + "\n\n")
	}
	if m.toolsLoading {
		b.WriteString(styleMeta.Render(m.spin.View()+" carregando…") + "\n\n")
		return b.String()
	}

	items := m.toolToggleItems()
	if len(items) == 0 {
		b.WriteString(styleHint.Render("(nada registrado)") + "\n")
	}

	kind := ""
	for i, it := range items {
		if it.kind != kind {
			kind = it.kind
			label := "tools"
			if kind == "skill" {
				label = "skills"
			}
			b.WriteString(styleMeta.Render(label) + "\n")
		}
		checkbox := "[x]"
		disabled := m.toolSettings != nil && m.toolSettings.IsDisabled(it.name)
		if disabled {
			checkbox = "[ ]"
		}
		desc := it.description
		if len(desc) > 60 {
			desc = desc[:60] + "…"
		}
		line := fmt.Sprintf("%s %-24s %s", checkbox, it.name, styleHint.Render(desc))
		if i == m.toolsCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\n")
	if m.toolsStatus != "" {
		b.WriteString(styleHint.Render(m.toolsStatus) + "\n\n")
	}
	b.WriteString(styleHint.Render("↑↓ escolher · espaço/enter liga/desliga · a liga tudo · d desliga tudo · esc volta"))
	return b.String()
}

func (m Model) handleToolsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.wizardChosenToolsPreset == "custom" {
			// Reconcile once more before leaving Custom. This prevents a quick
			// final toggle+Esc (or a transient earlier sync failure) from letting
			// the first session start with startup-time daemon settings.
			m.toolsStatus = "aplicando ao daemon atual…"
			m.wizardToolSettingsPending = true
			return m, syncToolSettingsCmd(m.daemon, m.toolSettings)
		}
		m.phase = phasePicker
		return m, nil
	case "up":
		if m.toolsCursor > 0 {
			m.toolsCursor--
		}
	case "down":
		items := m.toolToggleItems()
		if m.toolsCursor < len(items)-1 {
			m.toolsCursor++
		}
	case "enter", " ":
		items := m.toolToggleItems()
		if m.toolsCursor >= len(items) || m.toolSettings == nil {
			return m, nil
		}
		it := items[m.toolsCursor]
		nowDisabled := !m.toolSettings.IsDisabled(it.name)
		if err := m.toolSettings.SetDisabled(it.name, nowDisabled); err != nil {
			m.toolsStatus = "erro ao salvar: " + err.Error()
		} else if nowDisabled {
			m.toolsStatus = it.name + ": desligado."
		} else {
			m.toolsStatus = it.name + ": ligado."
		}
		return m, syncToolSettingsCmd(m.daemon, m.toolSettings)
	case "a", "d":
		if m.toolSettings == nil {
			return m, nil
		}
		names := toolToggleNames(m.toolToggleItems())
		disable := msg.String() == "d"
		if err := m.toolSettings.SetAllDisabled(names, disable); err != nil {
			m.toolsStatus = "erro ao salvar: " + err.Error()
		} else if disable {
			m.toolsStatus = fmt.Sprintf("%d desligados.", len(names))
		} else {
			m.toolsStatus = fmt.Sprintf("%d ligados.", len(names))
		}
		return m, syncToolSettingsCmd(m.daemon, m.toolSettings)
	}
	return m, nil
}

type toolSettingsUpdatedMsg struct{ err error }

func syncToolSettingsCmd(c *daemonclient.Client, settings *toolsettings.Store) tea.Cmd {
	return func() tea.Msg {
		if c == nil || settings == nil {
			return toolSettingsUpdatedMsg{err: fmt.Errorf("daemon or tool settings unavailable")}
		}
		disabledMap := settings.Disabled()
		names := make([]string, 0, len(disabledMap))
		for name := range disabledMap {
			names = append(names, name)
		}
		sort.Strings(names)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return toolSettingsUpdatedMsg{err: c.UpdateToolSettings(ctx, names)}
	}
}

func toolToggleNames(items []toolToggleItem) []string {
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.name
	}
	return names
}

type toolsListMsg struct {
	tools  []daemonclient.ToolInfo
	skills []daemonclient.Skill
	err    error
}

func fetchToolsCmd(c *daemonclient.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tools, skills, err := c.ListTools(ctx)
		return toolsListMsg{tools: tools, skills: skills, err: err}
	}
}
