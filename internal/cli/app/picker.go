package app

import (
	"fmt"
	"strings"
	"time"
)

// renderPicker draws the session picker: a "new session" row followed by
// every existing session (most recently active first), so closing the CLI
// never loses track of a conversation — you land back on the same list of
// durable sessions the daemon already owns.
func (m Model) renderPicker() string {
	var b strings.Builder
	b.WriteString(renderBanner(m.width, m.combo, m.workspace, len(m.sessionList)) + "\n\n")
	b.WriteString(styleBody.Render("sessões") + "\n\n")

	if m.titling {
		b.WriteString(styleMeta.Render("nova sessão — título:") + "\n")
		b.WriteString(m.newSessionText.View() + "\n\n")
		b.WriteString(styleHint.Render("enter confirma · esc cancela"))
		return b.String()
	}

	if m.pickerErr != nil {
		b.WriteString(styleErrBadge.Render("erro: "+m.pickerErr.Error()) + "\n\n")
	}
	if m.pickerBusy {
		b.WriteString(styleMeta.Render(m.spin.View()+" carregando…") + "\n\n")
	}

	newRow := "+ nova sessão"
	if m.pickerCursor == 0 {
		b.WriteString(styleYouTag.Render("▸ "+newRow) + "\n")
	} else {
		b.WriteString(styleMeta.Render("  "+newRow) + "\n")
	}

	if len(m.sessionList) == 0 && !m.pickerBusy {
		b.WriteString(styleHint.Render("  (nenhuma sessão existente ainda)") + "\n")
	}

	for i, sess := range m.sessionList {
		idx := i + 1
		title := sess.Title
		if title == "" {
			title = sess.ID
		}
		age := formatAge(sess.UpdatedAt)
		line := fmt.Sprintf("%s  %s", title, styleHint.Render(age))
		if idx == m.pickerCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\n" + styleHint.Render("↑↓ escolher · enter confirmar · a contas · f ferramentas · ctrl+c sair"))
	return b.String()
}

func formatAge(unixSeconds int64) string {
	d := time.Since(time.Unix(unixSeconds, 0))
	switch {
	case d < time.Minute:
		return "agora"
	case d < time.Hour:
		return fmt.Sprintf("%dmin atrás", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh atrás", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd atrás", int(d.Hours()/24))
	}
}
