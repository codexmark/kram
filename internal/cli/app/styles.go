package app

import "github.com/charmbracelet/lipgloss"

// Kram's own palette — not borrowed from opencode/Crush's soft-border
// look. Chat content is unstyled monospace; color is reserved for status
// (the footer, the panel) so it stays meaningful instead of decorative.
var (
	colorMuted  = lipgloss.Color("240")
	colorFaint  = lipgloss.Color("237")
	colorText   = lipgloss.Color("252")
	colorAccent = lipgloss.Color("111") // you
	colorOK     = lipgloss.Color("114") // success / closed breaker
	colorWarn   = lipgloss.Color("179") // half-open / retrying
	colorDanger = lipgloss.Color("167") // open breaker / failure

	styleYouTag   = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleKramTag  = lipgloss.NewStyle().Foreground(colorOK).Bold(true)
	styleBody     = lipgloss.NewStyle().Foreground(colorText)
	styleMeta     = lipgloss.NewStyle().Foreground(colorMuted)
	styleHint     = lipgloss.NewStyle().Foreground(colorFaint)
	styleErrBadge = lipgloss.NewStyle().Foreground(colorDanger)

	styleBadgeOK   = lipgloss.NewStyle().Foreground(colorOK)
	styleBadgeWarn = lipgloss.NewStyle().Foreground(colorWarn)
	styleBadgeBad  = lipgloss.NewStyle().Foreground(colorDanger)
	styleBadgeIdle = lipgloss.NewStyle().Foreground(colorMuted)

	stylePanelBG = lipgloss.NewStyle().Background(lipgloss.Color("235")).Foreground(colorText)
)
