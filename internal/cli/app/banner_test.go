package app

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestFullBannerMatchesSuppliedASCIIAndAlignment(t *testing.T) {
	wantK := strings.Join([]string{
		"███   ███            ╱██KRAM",
		"█   █████          ╱██KRAM",
		"██   ████        ╱██KRAM",
		"███   ███      ╱██KRAM",
		"████   ██KRAM ──────",
		"██   ████KRAM ────",
		"███   ███KRAM ─",
		"████   ██      ╲██KRAM",
		"█████   █        ╲██KRAM",
		"███   ███          ╲██KRAM",
		"█   █████            ╲██KRAM",
	}, "\n")
	if got := strings.Join(kramKRows, "\n"); got != wantK {
		t.Fatalf("retracting-legs K changed:\n%s", got)
	}
	wantWordmark := strings.Join([]string{
		"██╗  ██╗██████╗  █████╗ ███╗   ███╗",
		"██║ ██╔╝██╔══██╗██╔══██╗████╗ ████║",
		"█████╔╝ ██████╔╝███████║██╔████╔██║",
		"██╔═██╗ ██╔══██╗██╔══██║██║╚██╔╝██║",
		"██║  ██╗██║  ██║██║  ██║██║ ╚═╝ ██║",
		"╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝     ╚═╝",
	}, "\n")
	if got := strings.Join(kramWordmarkRows, "\n"); got != wantWordmark {
		t.Fatalf("ANSI Shadow wordmark changed:\n%s", got)
	}

	rows := fullBannerPlainRows()
	if len(rows) != 11 {
		t.Fatalf("row count = %d, want 11", len(rows))
	}
	for i, row := range rows {
		if lipgloss.Width(row.left) > kramKWidth {
			t.Fatalf("left row %d is %d cells, exceeds %d", i+1, lipgloss.Width(row.left), kramKWidth)
		}
		if row.wordmark != "" && lipgloss.Width(row.wordmark) != kramWordmarkWidth {
			t.Fatalf("wordmark row %d width = %d, want %d", i+1, lipgloss.Width(row.wordmark), kramWordmarkWidth)
		}
	}
	if rows[8].tagline != kramTagline {
		t.Fatalf("row nine tagline = %q, want %q", rows[8].tagline, kramTagline)
	}
	for i, row := range rows {
		if i != 8 && row.tagline != "" {
			t.Fatalf("tagline leaked onto row %d", i+1)
		}
	}
}

func TestBannerGradientUsesReferenceEndpoints(t *testing.T) {
	if got := bannerGradientColor(bannerPurple, bannerBlue, 0, kramKWidth); got != "#7842FF" {
		t.Errorf("left start = %s, want #7842FF", got)
	}
	if got := bannerGradientColor(bannerPurple, bannerBlue, kramKWidth-1, kramKWidth); got != "#4671FF" {
		t.Errorf("left end = %s, want #4671FF", got)
	}
	if got := bannerGradientColor(bannerBlue, bannerCyan, kramWordmarkWidth-1, kramWordmarkWidth); got != "#02ECFF" {
		t.Errorf("wordmark end = %s, want #02ECFF", got)
	}
}

func TestBannerNeverExceedsTerminalWidth(t *testing.T) {
	for _, width := range []int{30, bannerCompactThreshold, bannerFullThreshold, 120} {
		if got := lipgloss.Width(renderBanner(width)); got > width {
			t.Errorf("renderBanner(%d) width = %d", width, got)
		}
	}
}

func TestAnimatedBannerPreservesGeometryWhileRevealingAndDissolving(t *testing.T) {
	for _, state := range []struct {
		reveal float64
		fade   float64
	}{{0, 0}, {0.5, 0}, {1, 0}, {1, 0.5}, {1, 1}} {
		got := renderAnimatedBanner(120, state.reveal, state.fade)
		if width := lipgloss.Width(got); width != 120 {
			t.Errorf("animation (%v, %v) width = %d, want 120", state.reveal, state.fade, width)
		}
	}

	hidden := gradientTextAnimated("KRAM", bannerPurple, bannerCyan, 4, 0, 0,
		bannerAnimation{reveal: 0, totalWidth: 4})
	if hidden != "    " {
		t.Errorf("unrevealed wordmark = %q, want four occupied blank cells", hidden)
	}
	dissolved := gradientTextAnimated("KRAM", bannerPurple, bannerCyan, 4, 0, 0,
		bannerAnimation{reveal: 1, fade: 1, totalWidth: 4})
	if dissolved != "    " {
		t.Errorf("fully dissolved wordmark = %q, want four occupied blank cells", dissolved)
	}
}
