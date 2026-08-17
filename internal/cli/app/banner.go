package app

import "strings"

// kramBanner is a small block-letter "KRAM" wordmark, verified to render
// correctly before shipping (hand-aligned ASCII art is easy to get subtly
// wrong — this was built and checked column-by-column, not typed freehand
// into the final file). Three rows tall on purpose: discreet, not a
// full-screen splash — it's shown once, on the session picker, so you
// know at a glance which app just opened.
var kramBannerRows = []string{
	"█  █ █▀▀▄  ▄▄  █▄▄█",
	"█▄▄  █▄▄▀ █▄▄█ █▐▌█",
	"█  █ █  █ █  █ █  █",
}

// renderBanner returns the colored banner plus a muted tagline underneath.
func renderBanner() string {
	var b strings.Builder
	for _, row := range kramBannerRows {
		b.WriteString(styleBadgeAccent.Bold(true).Render(row) + "\n")
	}
	b.WriteString(styleHint.Render("agente de código local · confiável por padrão"))
	return b.String()
}
