package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// subagentSessionTitlePrefix mirrors internal/daemon/store's own
// subagentTitlePrefix: RunTask titles a delegate_task subagent's session
// "subagent: " + its goal, truncated to 60 runes (see agent.go's
// RunTask). Duplicated here rather than imported — the CLI only ever
// sees a session's title over the daemon's HTTP API, never the store
// package directly.
const subagentSessionTitlePrefix = "subagent: "

func isSubagentSessionTitle(title string) bool {
	return strings.HasPrefix(title, subagentSessionTitlePrefix)
}

// pickerVisibleSessions splits m.sessionList by whether it's a
// delegate_task subagent run, returning only the half m.pickerShowSubagents
// currently selects. Subagent sessions are excluded from the default
// (false) view entirely, matching session_search's own default
// SearchScopeUser exclusion (internal/daemon/store/search.go) — a session
// with dozens of delegations shouldn't bury real conversations in the
// picker. The "s" key (handlePickerKey) toggles which half is shown.
func (m Model) pickerVisibleSessions() []daemonclient.Session {
	var out []daemonclient.Session
	for _, sess := range m.sessionList {
		if isSubagentSessionTitle(sess.Title) == m.pickerShowSubagents {
			out = append(out, sess)
		}
	}
	return out
}

// pickerSubagentCount reports how many subagent sessions the default view
// is currently hiding — surfaced as a hint so their existence isn't a
// total blind spot even while they stay out of the main list.
func (m Model) pickerSubagentCount() int {
	count := 0
	for _, sess := range m.sessionList {
		if isSubagentSessionTitle(sess.Title) {
			count++
		}
	}
	return count
}

// renderPicker draws the session picker: a "new session" row followed by
// every existing session (most recently active first), so closing the CLI
// never loses track of a conversation — you land back on the same list of
// durable sessions the daemon already owns. delegate_task's own subagent
// sessions are a separate, opt-in view (see pickerVisibleSessions) rather
// than mixed into this default list.
func (m Model) renderPicker() string {
	var b strings.Builder
	title := pickerTitle
	if m.pickerShowSubagents {
		title = pickerSubagentTitle
	}
	b.WriteString(styleBody.Render(title) + "\n\n")

	if m.titling {
		b.WriteString(styleMeta.Render(pickerNewSessionLabel) + "\n")
		b.WriteString(m.newSessionText.View() + "\n\n")
		b.WriteString(styleHint.Render(pickerNewSessionHint))
		return b.String()
	}

	if m.pickerErr != nil {
		b.WriteString(styleErrBadge.Render(pickerErrPrefix+m.pickerErr.Error()) + "\n\n")
	}
	if m.pickerBusy {
		b.WriteString(styleMeta.Render(m.spin.View()+pickerLoading) + "\n\n")
	}

	newRow := pickerNewSessionRow
	if m.pickerCursor == 0 {
		b.WriteString(styleYouTag.Render("▸ "+newRow) + "\n")
	} else {
		b.WriteString(styleMeta.Render("  "+newRow) + "\n")
	}

	visible := m.pickerVisibleSessions()
	if len(visible) == 0 && !m.pickerBusy {
		empty := pickerEmptySessions
		if m.pickerShowSubagents {
			empty = pickerEmptySubagents
		}
		b.WriteString(styleHint.Render(empty) + "\n")
	}

	for i, sess := range visible {
		idx := i + 1
		label := sess.Title
		if label == "" {
			label = sess.ID
		}
		age := formatAge(sess.UpdatedAt)
		line := fmt.Sprintf("%s  %s", label, styleHint.Render(age))
		if idx == m.pickerCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}

	hint := pickerFooterMain
	if m.pickerShowSubagents {
		hint = pickerFooterSubagent
	} else if n := m.pickerSubagentCount(); n > 0 {
		hint = fmt.Sprintf(pickerSubagentCountFmt, n) + hint
	}
	b.WriteString("\n" + styleHint.Render(hint))
	return b.String()
}

func formatAge(unixSeconds int64) string {
	d := time.Since(time.Unix(unixSeconds, 0))
	switch {
	case d < time.Minute:
		return ageNow
	case d < time.Hour:
		return fmt.Sprintf(ageMinutesFmt, int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(ageHoursFmt, int(d.Hours()))
	default:
		return fmt.Sprintf(ageDaysFmt, int(d.Hours()/24))
	}
}
