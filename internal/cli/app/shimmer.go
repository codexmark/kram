package app

import (
	"math"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"
)

// shimmerPhasePerFrame is how far every sweeping-gradient animation's
// phase advances each animFrame tick (see commands.go's animTickCmd).
// Derived from animTickInterval rather than hardcoded, so the sweep's
// real-time speed (originally tuned as 0.35 radians per 120ms tick,
// ~2.9 rad/s) stays the same when the tick rate changes for smoothness —
// a faster tick alone would otherwise silently speed the whole animation
// up instead of just sampling it more often. Used by shimmerText,
// renderThinkingK, and routebar.go's own pulse dot.
var shimmerPhasePerFrame = 0.35 * animTickInterval.Seconds() / (120 * time.Millisecond).Seconds()

// activeStepFrames is how many animFrame ticks a cycling indicator's
// "active" node/letter (renderThinkingK, renderActivityRail, the route
// bar's own dot) dwells before advancing to the next one — same
// tick-rate-independent derivation as shimmerPhasePerFrame, keeping the
// original ~240ms-per-step cadence (2 frames at the 120ms baseline)
// instead of letting a faster tick rate speed up how often the "active"
// spot actually jumps, which is the part a faster tick alone can't smooth
// out (a letter is either uppercase or it isn't) — only the color sweep
// between jumps benefits from more frequent sampling.
var activeStepFrames = maxInt(1, int(240*time.Millisecond/animTickInterval))

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

// terminalColorProfile is a var, not a direct lipgloss.ColorProfile()
// call at each use site, so tests can force a specific tier without
// depending on whatever terminal actually runs `go test` — see
// shimmer_test.go's forced-profile cases.
var terminalColorProfile = lipgloss.ColorProfile

// supportsSmoothGradient reports whether the detected terminal can
// render the continuous per-character BlendLuv sweep meaningfully.
// TrueColor and ANSI256 have enough distinct steps for the sweep to read
// as a genuine gradient. Ascii already degrades every color to none at
// all, automatically, via lipgloss/termenv's own Profile.Convert — the
// smooth-vs-not question doesn't even apply there, since there is no
// color either way, with or without this check. Plain 16-color ANSI is
// the one real gap: it has colors, but a continuous sine blend quantized
// into only 16 buckets jumps between them unpredictably from one
// character (or frame) to the next — not a broken render, just a coarse,
// uncommitted-looking one where a deliberate two-color alternation reads
// better. See shimmerText/renderThinkingK for where this actually
// changes rendering.
func supportsSmoothGradient() bool {
	switch terminalColorProfile() {
	case termenv.TrueColor, termenv.ANSI256:
		return true
	default: // termenv.ANSI, termenv.Ascii
		return false
	}
}

// shimmerText renders text with a color wave sweeping across its
// characters, phase-shifted by frame so it animates as frame advances,
// on a terminal that can render it smoothly (see supportsSmoothGradient).
// On a plain 16-color or no-color terminal it falls back to a coarser,
// deliberate two-color alternation by character parity instead — still
// legibly "alive," without the per-character color jitter a quantized
// continuous blend would produce. Falls back to plain bold text if text
// is empty — never worth a panic over a cosmetic effect.
func shimmerText(text string, frame int) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return text
	}
	if !supportsSmoothGradient() {
		return shimmerTextCoarse(runes, frame)
	}
	var out string
	for i, r := range runes {
		// One full sine cycle sweeps across the text roughly every 2s,
		// regardless of animTickInterval — see shimmerPhasePerFrame.
		phase := float64(i)/float64(len(runes))*2*math.Pi + float64(frame)*shimmerPhasePerFrame
		blend := (math.Sin(phase) + 1) / 2 // 0..1
		c := shimmerFrom.BlendLuv(shimmerTo, blend)
		out += lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Bold(true).Render(string(r))
	}
	return out
}

// shimmerTextCoarse alternates each half of the text between the two
// shimmer endpoints as flat, unblended colors — still auto-downsampled
// to the nearest ANSI-16 color by lipgloss/termenv same as the smooth
// path, but with only two fixed colors instead of a continuous quantized
// sweep, so there's nothing to jitter. Which half is which color flips
// every few frames, keeping some motion without per-character noise.
func shimmerTextCoarse(runes []rune, frame int) string {
	flipped := (frame/6)%2 == 1
	mid := len(runes) / 2
	var out string
	for i, r := range runes {
		c := shimmerFrom
		if (i < mid) == flipped {
			c = shimmerTo
		}
		out += lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Bold(true).Render(string(r))
	}
	return out
}

// thinkingKPlain is the live indicator's base text: the product name
// itself, lowercase, so a single letter uppercasing per frame (see
// renderThinkingK) reads as "one letter growing" rather than shouting the
// whole word. Kept as its own function (not just an inline literal) so
// tests have one place to pull the base word from — same shape the
// previous Braille-K glyph's own thinkingKPlain had.
func thinkingKPlain() string {
	return "kram"
}

// renderThinkingK renders the live indicator: "kram" with exactly one
// letter uppercased and bold per frame, cycling K→r→a→m→(repeat) — "each
// letter grows once" — while the color gradient sweep (BlendLuv smooth
// path, or the two-color coarse fallback on limited-color terminals)
// keeps moving across all four letters independently of which one is
// currently emphasized, exactly as it already did for the previous
// two-point Braille glyph this replaced.
func renderThinkingK(frame int, stalled bool) string {
	letters := []rune(thinkingKPlain())
	if stalled {
		return styleBadgeWarn.Bold(true).Render(string(letters))
	}

	active := positiveModulo(frame/activeStepFrames, len(letters))
	smooth := supportsSmoothGradient()
	result := ""
	for i, r := range letters {
		display := r
		if i == active {
			display = unicode.ToUpper(r)
		}
		var color colorful.Color
		if smooth {
			// Same continuous per-character sweep shimmerText itself uses,
			// not the old 2-point glyph's harder i*math.Pi alternation —
			// four letters read as a genuine moving gradient this way
			// instead of a hard two-color checkerboard.
			phase := float64(i)/float64(len(letters))*2*math.Pi + float64(frame)*shimmerPhasePerFrame
			blend := (math.Sin(phase) + 1) / 2
			color = shimmerFrom.BlendLuv(shimmerTo, blend)
		} else {
			// Same reasoning as shimmerTextCoarse: on a 16-color terminal,
			// two fixed colors read more deliberately than a quantized
			// sweep, which has just as much room to jitter as a longer one
			// despite the smaller rune count.
			color = shimmerFrom
			if i%2 == 1 {
				color = shimmerTo
			}
		}
		style := lipgloss.NewStyle().Foreground(lipgloss.Color(color.Hex())).Bold(i == active)
		result += style.Render(string(display))
	}
	return result
}

func positiveModulo(value, modulus int) int {
	if modulus <= 0 {
		return 0
	}
	result := value % modulus
	if result < 0 {
		result += modulus
	}
	return result
}
