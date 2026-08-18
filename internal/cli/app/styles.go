package app

import "github.com/charmbracelet/lipgloss"

// Kram's own palette — not borrowed from opencode/Crush's soft-border
// look. Chat content is unstyled monospace; color is reserved for status
// (the footer, the panel) so it stays meaningful instead of decorative.
var (
	colorMuted    = lipgloss.Color("240")
	colorFaint    = lipgloss.Color("237")
	colorText     = lipgloss.Color("252")
	colorAccent   = lipgloss.Color("111") // you
	colorOK       = lipgloss.Color("114") // success / closed breaker
	colorWarn     = lipgloss.Color("179") // half-open / retrying
	colorDanger   = lipgloss.Color("167") // open breaker / failure
	colorMemory   = lipgloss.Color("183") // cross-session memory
	colorWordmark = lipgloss.Color("212") // the full-width FIGlet banner

	styleYouTag   = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleKramTag  = lipgloss.NewStyle().Foreground(colorOK).Bold(true)
	styleBody     = lipgloss.NewStyle().Foreground(colorText)
	styleMeta     = lipgloss.NewStyle().Foreground(colorMuted)
	styleHint     = lipgloss.NewStyle().Foreground(colorFaint)
	styleErrBadge = lipgloss.NewStyle().Foreground(colorDanger)

	styleBadgeOK     = lipgloss.NewStyle().Foreground(colorOK)
	styleBadgeWarn   = lipgloss.NewStyle().Foreground(colorWarn)
	styleBadgeBad    = lipgloss.NewStyle().Foreground(colorDanger)
	styleBadgeIdle   = lipgloss.NewStyle().Foreground(colorMuted)
	styleBadgeAccent = lipgloss.NewStyle().Foreground(colorAccent)
	styleBadgeMemory = lipgloss.NewStyle().Foreground(colorMemory)

	// styleUserBody colors the user's own message text distinctly from
	// kram's replies (both used to share styleBody, leaving only the tiny
	// "you"/"kram" tag to tell them apart at a glance) — same accent hue
	// as the "you" tag itself, so a whole user turn reads as one color.
	styleUserBody = lipgloss.NewStyle().Foreground(colorAccent)

	// styleWordmark colors the full-width FIGlet banner — the exact
	// magenta the user's own reference art used, not the accent blue
	// used everywhere else, since this one mark is meant to stand out as
	// a splash rather than blend into the rest of the status-color system.
	styleWordmark = lipgloss.NewStyle().Foreground(colorWordmark).Bold(true)

	styleFaintTrack = lipgloss.NewStyle().Foreground(colorFaint)

	stylePanelBG = lipgloss.NewStyle().Background(lipgloss.Color("235")).Foreground(colorText)
)
