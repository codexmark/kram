package app

import (
	"math/bits"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

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
