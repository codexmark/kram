package app

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderPromptBlockShrinksToShortContent(t *testing.T) {
	m := Model{width: 100}
	m.viewport.Width = 100
	got := m.renderPromptBlock("hi")
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least a label line and a content line, got %q", got)
	}
	contentLine := lines[1]
	// The block itself (bar + content, ignoring the right-alignment
	// margin that legitimately pads the line out to the terminal edge)
	// must not stretch out to anywhere near the 68-cols max width just
	// because that's the wrap limit — this is the specific
	// lipgloss.Style.Width() pitfall (pads short content with trailing
	// spaces) DECISIONS.md already documents once.
	trimmed := strings.TrimLeft(contentLine, " ")
	if w := lipgloss.Width(trimmed); w > 10 {
		t.Errorf("a two-character message produced a %d-cell-wide block (margin stripped), expected it to shrink to content: %q", w, trimmed)
	}
}

func TestRenderPromptBlockRightAligned(t *testing.T) {
	m := Model{width: 80}
	m.viewport.Width = 80
	got := m.renderPromptBlock("short")
	lines := strings.Split(got, "\n")
	contentLine := lines[len(lines)-1]
	// The block's right edge should reach (or nearly reach) the terminal
	// width — it's right-aligned, not left-aligned or centered.
	if w := lipgloss.Width(contentLine); w < 60 {
		t.Errorf("expected the block to be right-aligned near the terminal's right edge (width ~80), got a %d-cell line: %q", w, contentLine)
	}
}

func TestRenderPromptBlockWrapsLongContent(t *testing.T) {
	m := Model{width: 60}
	m.viewport.Width = 60
	long := strings.Repeat("palavra ", 30) // way longer than any reasonable wrap width
	got := m.renderPromptBlock(long)
	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Errorf("expected long content to wrap across multiple lines, got %d lines", len(lines))
	}
	// No line should exceed the terminal width.
	for _, l := range lines {
		if w := lipgloss.Width(l); w > m.width {
			t.Errorf("prompt block line exceeds terminal width %d: %d cells (%q)", m.width, w, l)
		}
	}
}

func TestRenderPromptBlockPreservesAllContent(t *testing.T) {
	// Wrapping must never drop characters — every word from the original
	// message should still appear somewhere in the rendered block.
	m := Model{width: 50}
	m.viewport.Width = 50
	msg := "implemente essa função sem alterar a API pública"
	got := m.renderPromptBlock(msg)
	for _, word := range strings.Fields(msg) {
		if !strings.Contains(got, word) {
			t.Errorf("word %q from the original message is missing from the rendered block", word)
		}
	}
}

func TestRenderPromptBlockNarrowTerminalDegradesGracefully(t *testing.T) {
	m := Model{width: 30}
	m.viewport.Width = 30
	got := m.renderPromptBlock("mensagem razoavelmente longa para um terminal estreito")
	lines := strings.Split(got, "\n")
	for _, l := range lines {
		if w := lipgloss.Width(l); w > m.width {
			t.Errorf("narrow terminal (width=%d): a rendered line is %d cells wide: %q", m.width, w, l)
		}
	}
}

func TestRenderPromptBlockShowsYouLabel(t *testing.T) {
	m := Model{width: 80}
	m.viewport.Width = 80
	got := m.renderPromptBlock("test")
	if !strings.Contains(got, "you") {
		t.Error("expected the block to carry a \"you\" label")
	}
}
