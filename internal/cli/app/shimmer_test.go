package app

import (
	"math/bits"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
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

func TestThinkingKIsDenseAndSingleLine(t *testing.T) {
	plain := thinkingKPlain()
	if plain != "⡧⡎" {
		t.Fatalf("thinking K = %q, want compact Braille K", plain)
	}
	if width := lipgloss.Width(plain); width != 2 {
		t.Fatalf("thinking K width = %d, want 2", width)
	}
	dots := 0
	for _, r := range plain {
		dots += bits.OnesCount(uint(r - 0x2800))
	}
	if dots != 9 {
		t.Fatalf("thinking K dots = %d, want 9", dots)
	}
	for frame := -1; frame < 12; frame++ {
		if width := lipgloss.Width(renderThinkingK(frame, false)); width != 2 {
			t.Fatalf("frame %d width = %d, want 2", frame, width)
		}
	}
}

func TestThinkingKStalledStateAndModuloEdges(t *testing.T) {
	if got := renderThinkingK(3, true); !strings.Contains(got, thinkingKPlain()) {
		t.Fatalf("stalled K lost its silhouette: %q", got)
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

// TestRenderThinkingKStaysTwoRunesOnLimitedColorTerminals mirrors
// TestThinkingKIsDenseAndSingleLine's own width check, but forced onto
// the coarse rendering path — the K's silhouette must survive the
// fallback exactly like it does the smooth path.
func TestRenderThinkingKStaysTwoRunesOnLimitedColorTerminals(t *testing.T) {
	withColorProfile(t, termenv.ANSI)
	for frame := -1; frame < 12; frame++ {
		got := renderThinkingK(frame, false)
		if width := lipgloss.Width(got); width != 2 {
			t.Fatalf("frame %d width = %d, want 2 (ANSI-tier fallback)", frame, width)
		}
		if !strings.Contains(got, thinkingKPlain()) {
			t.Fatalf("frame %d lost the K silhouette on the ANSI-tier fallback: %q", frame, got)
		}
	}
}

func TestThinkingLineDistinguishesProgressFromStall(t *testing.T) {
	now := time.Now()
	working := Model{waitStartedAt: now.Add(-2 * time.Second), lastEventAt: now, animFrame: 2, workState: workModelActive}
	if got := working.thinkingLine(); !strings.Contains(got, thinkingKPlain()) || !strings.Contains(got, "MODELO ATIVO") {
		t.Fatalf("working line = %q", got)
	}

	stalled := Model{waitStartedAt: now.Add(-10 * time.Second), lastEventAt: now.Add(-stallThreshold - time.Second)}
	if got := stalled.thinkingLine(); !strings.Contains(got, thinkingKPlain()) || !strings.Contains(got, "CONEXÃO SEM EVENTOS") {
		t.Fatalf("stalled line = %q", got)
	}
}
