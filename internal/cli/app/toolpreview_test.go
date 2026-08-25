package app

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// TestStreamingContentWrapsToNarrowViewport is the regression test for a
// real, pre-existing bug the user hit in practice: a still-streaming
// message's plain-text content was rendered with no width constraint at
// all (unlike the finished-message markdown path, which glamour already
// wraps at m.mdRenderer's configured width) — bubbles' own viewport
// clips an overflowing line rather than wrapping it, so a long streaming
// line used to just get cut off once the Ctrl+B process pane narrowed
// the chat column.
func TestStreamingContentWrapsToNarrowViewport(t *testing.T) {
	m := testModel(t)
	m.viewport.Width = 20
	m.viewport.Height = 30 // room for every wrapped line, so View() doesn't scroll any out
	m.messages = []chatMessage{{Role: "assistant", streaming: true, Content: strings.Repeat("word ", 30)}}
	m.refreshTranscript()

	content := m.viewport.View()
	for _, line := range strings.Split(content, "\n") {
		if width := lipgloss.Width(line); width > m.viewport.Width {
			t.Fatalf("rendered line is %d columns wide, want it to fit within the %d-wide viewport: %q", width, m.viewport.Width, line)
		}
	}
	// The real bug: without wrapping, bubbles' own viewport clips an
	// overflowing line instead of wrapping it — words past the width
	// vanish from what's displayed entirely, not just visually cramped.
	if got := strings.Count(content, "word"); got != 30 {
		t.Fatalf("rendered content has %d occurrences of \"word\", want all 30 to survive wrapping — got: %q", got, content)
	}
}

func TestRenderToolResultPreviewEmptyWhenRunningOrBackgroundOrNoResult(t *testing.T) {
	m := testModel(t)
	cases := []daemonclient.ToolActivity{
		{Name: "bash", Running: true, Result: "still going"},
		{Name: "run_background", ProcessID: "bg1", Result: "server started"},
		{Name: "bash", Result: ""},
		{Name: "bash", Result: "   \n  "},
	}
	for _, act := range cases {
		if got := m.renderToolResultPreview(act); got != "" {
			t.Errorf("renderToolResultPreview(%+v) = %q, want empty", act, got)
		}
	}
}

func TestRenderToolResultPreviewShowsShortResult(t *testing.T) {
	m := testModel(t)
	act := daemonclient.ToolActivity{Name: "bash", Result: "line one\nline two", OK: true}
	got := m.renderToolResultPreview(act)
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("preview = %q, want both lines present", got)
	}
	if strings.Contains(got, "linhas") {
		t.Errorf("preview = %q, want no overflow suffix for a short result", got)
	}
}

func TestRenderToolResultPreviewTruncatesLongOutputWithLineCount(t *testing.T) {
	m := testModel(t)
	lines := make([]string, toolResultPreviewMaxLines+3)
	for i := range lines {
		lines[i] = "output line"
	}
	act := daemonclient.ToolActivity{Name: "bash", Result: strings.Join(lines, "\n")}
	got := m.renderToolResultPreview(act)
	if !strings.Contains(got, "+3 linhas") {
		t.Errorf("preview = %q, want a \"+3 linhas\" overflow suffix", got)
	}
	if strings.Count(got, "output line") != toolResultPreviewMaxLines {
		t.Errorf("preview shown lines = %d, want exactly %d", strings.Count(got, "output line"), toolResultPreviewMaxLines)
	}
}

// TestRenderToolResultPreviewTruncatesWideLineToViewportWidth is the
// regression test for the real bug a user hit in practice: a fixed
// width cap wider than the actual (narrowed, e.g. by the Ctrl+B tile)
// viewport used to get silently clipped by bubbles' own viewport instead
// of truncated with an ellipsis. Forces a narrow viewport and confirms
// the preview line never exceeds it.
func TestRenderToolResultPreviewTruncatesWideLineToViewportWidth(t *testing.T) {
	m := testModel(t)
	m.viewport.Width = 40
	act := daemonclient.ToolActivity{Name: "bash", Result: strings.Repeat("x", 200)}
	got := m.renderToolResultPreview(act)
	if !strings.Contains(got, "…") {
		t.Errorf("preview = %q, want an ellipsis marking the truncated line", got)
	}
	visibleLine := strings.TrimPrefix(got, toolResultPreviewIndent)
	if width := len([]rune(visibleLine)); width > m.viewport.Width {
		t.Errorf("preview line is %d runes wide, want it to fit within the %d-wide viewport", width, m.viewport.Width)
	}
}

func TestRenderToolResultPreviewStripsANSI(t *testing.T) {
	m := testModel(t)
	act := daemonclient.ToolActivity{Name: "bash", Result: "\x1b[31mred text\x1b[0m"}
	got := m.renderToolResultPreview(act)
	if !strings.Contains(got, "red text") || strings.Contains(got, "\x1b[31m") {
		t.Errorf("preview = %q, want ANSI stripped from the raw tool output", got)
	}
}

func TestRenderToolActivityAppendsPreviewWhenResultPresent(t *testing.T) {
	m := testModel(t)
	act := daemonclient.ToolActivity{Name: "bash", Args: "ls", Result: "file1.go\nfile2.go", OK: true}
	got := m.renderToolActivity(act)
	if !strings.Contains(got, "bash(ls)") {
		t.Errorf("renderToolActivity missing the name(args) line: %q", got)
	}
	if !strings.Contains(got, "file1.go") {
		t.Errorf("renderToolActivity missing the result preview: %q", got)
	}
}

func TestRenderToolActivityRunningToolHasNoPreview(t *testing.T) {
	m := testModel(t)
	act := daemonclient.ToolActivity{Name: "bash", Args: "sleep 5", Running: true}
	got := m.renderToolActivity(act)
	if strings.Contains(got, "\n") {
		t.Errorf("renderToolActivity for a still-running tool call = %q, want a single line (no result to preview yet)", got)
	}
}

// TestRenderToolActivityArgsTruncatesToNarrowViewport is the regression
// test for the same class of bug as the preview one above, but for the
// name(args) line itself: a fixed 60-rune cap used to overflow (and get
// clipped, not wrapped, by bubbles' viewport) whenever the actual
// viewport was narrower than that — exactly what happens once the
// Ctrl+B process pane tiles the chat column.
func TestRenderToolActivityArgsTruncatesToNarrowViewport(t *testing.T) {
	m := testModel(t)
	m.viewport.Width = 30
	act := daemonclient.ToolActivity{Name: "bash", Args: strings.Repeat("a", 200), OK: true}
	got := m.renderToolActivity(act)
	firstLine := strings.SplitN(got, "\n", 2)[0]
	if width := len([]rune(firstLine)); width > m.viewport.Width+2 { // small slack for styled glyphs (✓/↳) lipgloss.Width already accounts for
		t.Errorf("tool activity line is %d runes wide, want it to fit within the %d-wide viewport", width, m.viewport.Width)
	}
}
