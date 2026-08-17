package app

import (
	"math"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
)

// shimmerFrom/To are the gradient endpoints for the "working" shimmer —
// a genuine per-character color interpolation (via go-colorful) rather
// than the earlier discrete 4-color palette jump. This is the single
// most-repeated technique in the visual research (both Crush's
// WorkingGrad and OpenClaude's GlimmerMessage do the same thing
// independently): a moving gradient reads as "alive" in a way a static
// spinner glyph doesn't.
var (
	shimmerFrom, _ = colorful.Hex("#7c9eff") // cool blue, echoes styleBadgeAccent
	shimmerTo, _   = colorful.Hex("#5fd7a7") // warm green, echoes styleBadgeOK
)

// shimmerText renders text with a color wave sweeping across its
// characters, phase-shifted by frame so it animates as frame advances.
// Falls back to plain bold text if text is empty — never worth a panic
// over a cosmetic effect.
func shimmerText(text string, frame int) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return text
	}
	var out string
	for i, r := range runes {
		// One full sine cycle sweeps across the text roughly every 2s at
		// the 120ms tick rate (animFrame increments once per tick).
		phase := float64(i)/float64(len(runes))*2*math.Pi + float64(frame)*0.35
		blend := (math.Sin(phase) + 1) / 2 // 0..1
		c := shimmerFrom.BlendLuv(shimmerTo, blend)
		out += lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Bold(true).Render(string(r))
	}
	return out
}
