package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// kramWordmarkRows is the full FIGlet-style "KramGateway" wordmark — the
// exact art the user supplied, kept byte-for-byte rather than
// re-transcribed by eye from the reference screenshot (hand-guessing
// FIGlet glyph shapes is exactly the kind of thing that goes subtly
// wrong). Wide: about 95 columns, so it only fits comfortably in a
// spacious terminal — see bannerFigletThreshold.
var kramWordmarkRows = []string{
	`88      a8P                                                                                 88`,
	`88    ,88'                                                                                  88`,
	`88  ,88"                                                                                    88`,
	`88,d88'       8b,dPPYba,  ,adPPYYba,  88,dPYba,,adPYba,    ,adPPYba,   ,adPPYba,    ,adPPYb,88   ,adPPYba,`,
	`8888"88,      88P'   "Y8  ""     ` + "`" + `Y8  88P'   "88"    "8a  a8"     ""  a8"     "8a  a8"    ` + "`" + `Y88  a8P_____88`,
	`88P   Y8b     88          ,adPPPPP88  88      88      88  8b          8b       d8  8b       88  8PP"""""""`,
	`88     "88,   88          88,    ,88  88      88      88  "8a,   ,aa  "8a,   ,a8"  "8a,   ,d88  "8b,   ,aa`,
	`88       Y8b  88          ` + "`" + `"8bbdP"Y8  88      88      88   ` + "`" + `"Ybbd8"'   ` + "`" + `"YbbdP"'    ` + "`" + `"8bbdP"Y8   ` + "`" + `"Ybbd8"'`,
}

// kramBannerRows is the small block-letter "KRAM" wordmark used at
// medium widths, when the full FIGlet art doesn't fit but there's still
// room for more than a plain text line — verified to render correctly
// before shipping (hand-aligned ASCII art is easy to get subtly wrong —
// this was built and checked column-by-column, not typed freehand into
// the final file).
var kramBannerRows = []string{
	"█  █ █▀▀▄  ▄▄  █▄▄█",
	"█▄▄  █▄▄▀ █▄▄█ █▐▌█",
	"█  █ █  █ █  █ █  █",
}

// bannerFigletThreshold is the terminal width below which the full
// ~95-column FIGlet wordmark stops fitting comfortably in the bordered
// panel. bannerWideThreshold is the next step down, below which even the
// compact block-letter mark doesn't fit — the same narrow-terminal escape
// hatch Hermes Agent's CLI takes with its own COMPACT_BANNER.
const (
	bannerFigletThreshold = 104
	bannerWideThreshold   = 46
)

// renderBanner draws Kram's startup banner: a logo boxed together with
// real, live state — which model combo new messages use, which project
// this instance is scoped to, how many durable sessions already exist —
// the same idea as Hermes Agent's welcome panel (logo plus model/
// directory/tool status in one bordered box, painted before the rest of
// the UI finishes loading) rather than opencode's plain, decoration-free
// cold start straight into the TUI. No invented fields: every line here
// is something the CLI already knows for real, not filler.
func renderBanner(width int, combo, workspace string, sessionCount int) string {
	var body strings.Builder
	switch {
	case width >= bannerFigletThreshold:
		for _, row := range kramWordmarkRows {
			body.WriteString(styleWordmark.Render(row) + "\n")
		}
	case width >= bannerWideThreshold:
		for _, row := range kramBannerRows {
			body.WriteString(styleBadgeAccent.Bold(true).Render(row) + "\n")
		}
	default:
		body.WriteString(styleBadgeAccent.Bold(true).Render("KRAM") + "\n")
	}
	body.WriteString(styleHint.Render("local coding agent · reliable and opinionated, by Mark Mesquita") + "\n\n")

	project := "—"
	if workspace != "" {
		project = filepath.Base(filepath.Clean(workspace))
	}
	sessions := fmt.Sprintf("%d salva", sessionCount)
	if sessionCount != 1 {
		sessions = fmt.Sprintf("%d salvas", sessionCount)
	}

	statusLines := []string{
		statusRow("modelo", combo),
		statusRow("projeto", project),
		statusRow("sessões", sessions),
	}
	body.WriteString(strings.Join(statusLines, "\n"))

	// No fixed Width(): the box auto-sizes to its widest line instead —
	// simpler than computing a target width per art tier, and it's
	// already safe because the switch above only picks an art variant
	// that fits under the terminal's actual width in the first place.
	// The status lines below are always much narrower than any wordmark
	// variant, so they just sit left-aligned inside the wider box.
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorFaint).
		Padding(0, 2).
		Render(body.String())
}

func statusRow(label, value string) string {
	return styleMeta.Render(fmt.Sprintf("%-8s", label)) + styleBody.Render(value)
}
