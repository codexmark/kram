package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// toolToggleItem is one row on the tools/skills screen — a built-in tool
// or a discovered skill, whichever the daemon reported. Name/description
// come from the daemon (it's the only thing that actually knows what's
// registered); on/off state comes from the CLI's own local
// toolsettings.Store, same split as the accounts screen: mutate here,
// takes effect on the daemon's next restart.
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
	b.WriteString(styleHint.Render("↑↓ escolher · espaço/enter liga/desliga · esc volta (reinicie o kram pra aplicar)"))
	return b.String()
}

func (m Model) handleToolsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
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
	}
	return m, nil
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
