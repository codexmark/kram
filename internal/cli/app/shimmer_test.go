package app

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// withColorProfile forces terminalColorProfile for the duration of the
// test, restoring it after — the only way to exercise
// supportsSmoothGradient's non-default branches deterministically,
// since the real detected profile depends on whatever terminal actually
// runs `go test`.
func withColorProfile(t *testing.T, profile termenv.Profile) {
	t.Helper()
	orig := terminalColorProfile
	terminalColorProfile = func() termenv.Profile { return profile }
	t.Cleanup(func() { terminalColorProfile = orig })
}

// TestThinkingKSpellsKramWithOneLetterUppercasedPerFrame confirms the
// live indicator is the literal wordmark "kram" with exactly one letter
// uppercased (the "growing" letter) per frame, cycling through all four —
// the ANSI styling that colors/bolds individual letters differently
// means the rendered word can't be asserted as one contiguous plain
// substring, so this strips ANSI first (ansi.Strip) rather than
// string-containing the raw styled output.
func TestThinkingKSpellsKramWithOneLetterUppercasedPerFrame(t *testing.T) {
	if plain := thinkingKPlain(); plain != "kram" {
		t.Fatalf("thinking wordmark = %q, want %q", plain, "kram")
	}
	for frame := -1; frame < 12; frame++ {
		visible := ansi.Strip(renderThinkingK(frame, false))
		if strings.ToLower(visible) != "kram" {
			t.Fatalf("frame %d rendered %q, want it to spell kram (case-insensitive)", frame, visible)
		}
		if width := lipgloss.Width(visible); width != 4 {
			t.Fatalf("frame %d width = %d, want 4", frame, width)
		}
		upper := 0
		for _, r := range visible {
			if unicode.IsUpper(r) {
				upper++
			}
		}
		if upper != 1 {
			t.Fatalf("frame %d has %d uppercase letters in %q, want exactly 1", frame, upper, visible)
		}
	}
}

func TestThinkingKStalledStateAndModuloEdges(t *testing.T) {
	if got := renderThinkingK(3, true); !strings.Contains(got, thinkingKPlain()) {
		t.Fatalf("stalled indicator lost its wordmark: %q", got)
	}
	if got := positiveModulo(-1, 2); got != 1 {
		t.Fatalf("positiveModulo(-1, 2) = %d, want 1", got)
	}
	if got := positiveModulo(10, 0); got != 0 {
		t.Fatalf("positiveModulo with zero modulus = %d, want 0", got)
	}
}

func TestSupportsSmoothGradientByProfile(t *testing.T) {
	cases := []struct {
		profile termenv.Profile
		want    bool
	}{
		{termenv.TrueColor, true},
		{termenv.ANSI256, true},
		{termenv.ANSI, false},
		{termenv.Ascii, false},
	}
	for _, tc := range cases {
		withColorProfile(t, tc.profile)
		if got := supportsSmoothGradient(); got != tc.want {
			t.Errorf("supportsSmoothGradient() with profile %v = %v, want %v", tc.profile, got, tc.want)
		}
	}
}

// TestShimmerTextDegradesOnLimitedColorTerminals confirms the coarse
// fallback actually engages on ANSI/Ascii — not just that
// supportsSmoothGradient reports the right bool in isolation — and that
// it never breaks the text itself (same visible runes, still styled).
func TestShimmerTextDegradesOnLimitedColorTerminals(t *testing.T) {
	for _, profile := range []termenv.Profile{termenv.ANSI, termenv.Ascii} {
		withColorProfile(t, profile)
		got := shimmerText("kram", 5)
		if !strings.Contains(got, "k") || !strings.Contains(got, "m") {
			t.Errorf("profile %v: shimmerText lost characters, got %q", profile, got)
		}
		if lipgloss.Width(got) != 4 {
			t.Errorf("profile %v: shimmerText width = %d, want 4", profile, lipgloss.Width(got))
		}
	}
}

func TestShimmerTextEmptyInputReturnsEmpty(t *testing.T) {
	if got := shimmerText("", 0); got != "" {
		t.Errorf("shimmerText(\"\", 0) = %q, want empty", got)
	}
}

// TestRenderThinkingKStaysFourRunesOnLimitedColorTerminals mirrors
// TestThinkingKSpellsKramWithOneLetterUppercasedPerFrame's own checks,
// but forced onto the coarse rendering path — the wordmark must survive
// the fallback exactly like it does the smooth path.
func TestRenderThinkingKStaysFourRunesOnLimitedColorTerminals(t *testing.T) {
	withColorProfile(t, termenv.ANSI)
	for frame := -1; frame < 12; frame++ {
		visible := ansi.Strip(renderThinkingK(frame, false))
		if width := lipgloss.Width(visible); width != 4 {
			t.Fatalf("frame %d width = %d, want 4 (ANSI-tier fallback)", frame, width)
		}
		if strings.ToLower(visible) != "kram" {
			t.Fatalf("frame %d lost the wordmark on the ANSI-tier fallback: %q", frame, visible)
		}
	}
}

func TestThinkingLineDistinguishesProgressFromStall(t *testing.T) {
	now := time.Now()
	working := Model{waitStartedAt: now.Add(-2 * time.Second), lastEventAt: now, animFrame: 2, workState: workModelActive}
	got := working.thinkingLine()
	if !strings.Contains(strings.ToLower(ansi.Strip(got)), thinkingKPlain()) || !strings.Contains(got, "MODEL ACTIVE") {
		t.Fatalf("working line = %q", got)
	}

	stalled := Model{waitStartedAt: now.Add(-10 * time.Second), lastEventAt: now.Add(-stallThreshold - time.Second)}
	if got := stalled.thinkingLine(); !strings.Contains(got, thinkingKPlain()) || !strings.Contains(got, "NO STREAM EVENTS") {
		t.Fatalf("stalled line = %q", got)
	}
}
