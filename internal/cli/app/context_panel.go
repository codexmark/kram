package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// categoryLabel and categoryStyle map the daemon's real context categories
// (internal/daemon/agent.ContextUsage) to display text and color — no
// invented categories like "skills" or "MCP tools" that Kram doesn't have.
var categoryLabels = map[string]string{
	"messages":           ctxCatMessages,
	"system_prompt":      ctxCatSystemPrompt,
	"tool_overview":      ctxCatToolOverview,
	"tool_definitions":   ctxCatToolDefinitions,
	"project_context":    ctxCatProjectContext,
	"memory":             ctxCatMemory,
	"compaction_summary": ctxCatCompactionSummary,
	"response_reserve":   ctxCatResponseReserve,
	"free":               ctxCatFree,
}

func categoryStyle(name string) lipgloss.Style {
	switch name {
	case "messages":
		return styleBadgeAccent
	case "tool_definitions", "tool_overview", "response_reserve":
		return styleBadgeWarn
	case "project_context":
		return styleBadgeOK
	case "memory":
		return styleBadgeMemory
	case "compaction_summary":
		return styleBadgeBad
	default: // free
		return styleBadgeIdle
	}
}

// renderContextPanel draws the context-window usage panel: a segmented bar
// plus a breakdown row per category, all computed by the daemon the same
// way it decides when to compact (see internal/daemon/compaction) — the
// panel and the actual compaction trigger can never disagree.
func (m Model) renderContextPanel() string {
	h := m.panelHeight()
	var lines []string

	if m.contextErr != nil {
		lines = append(lines, styleErrBadge.Render(contextReadErrPrefix+m.contextErr.Error()))
		return padLines(lines, h, m.width)
	}
	if !m.haveContext || m.contextData.Budget <= 0 {
		lines = append(lines, styleMeta.Render(contextLoading))
		return padLines(lines, h, m.width)
	}

	u := m.contextData
	pct := u.Used * 100 / u.Budget

	lines = append(lines, styleMeta.Render(fmt.Sprintf(contextWindowLine, formatTokens(u.Used), formatTokens(u.Budget), pct)))
	lines = append(lines, contextBar(u, m.width-2))
	lines = append(lines, "")

	for _, c := range u.Categories {
		if c.Tokens == 0 {
			continue
		}
		label := categoryLabels[c.Name]
		if label == "" {
			label = c.Name
		}
		swatch := categoryStyle(c.Name).Render("■")
		catPct := ""
		if u.Budget > 0 {
			catPct = fmt.Sprintf("%.1f%%", float64(c.Tokens)*100/float64(u.Budget))
		}
		left := swatch + " " + styleBody.Render(label)
		right := styleMeta.Render(formatTokens(c.Tokens)) + "  " + styleHint.Render(catPct)
		lines = append(lines, padBetween(m.width-4, left, right))
	}

	return padLines(lines, h, m.width)
}

// contextBar draws a proportional, multi-color segmented bar summarizing
// every category in one line.
func contextBar(u daemonclient.ContextUsage, width int) string {
	if width < 10 {
		width = 10
	}
	var b strings.Builder
	drawn := 0
	for _, c := range u.Categories {
		if c.Tokens == 0 || u.Budget == 0 {
			continue
		}
		segWidth := c.Tokens * width / u.Budget
		if segWidth == 0 && c.Tokens > 0 {
			segWidth = 1
		}
		if drawn+segWidth > width {
			segWidth = width - drawn
		}
		if segWidth <= 0 {
			continue
		}
		b.WriteString(categoryStyle(c.Name).Render(strings.Repeat("█", segWidth)))
		drawn += segWidth
	}
	if drawn < width {
		b.WriteString(styleFaintTrack.Render(strings.Repeat("░", width-drawn)))
	}
	return b.String()
}

// formatTokens renders a token count the way the reference design does:
// "462.9k" for large numbers, plain integers below 1000.
func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
