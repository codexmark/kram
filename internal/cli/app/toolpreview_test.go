package app

import (
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

func TestRenderToolResultPreviewEmptyWhenRunningOrBackgroundOrNoResult(t *testing.T) {
	cases := []daemonclient.ToolActivity{
		{Name: "bash", Running: true, Result: "still going"},
		{Name: "run_background", ProcessID: "bg1", Result: "server started"},
		{Name: "bash", Result: ""},
		{Name: "bash", Result: "   \n  "},
	}
	for _, act := range cases {
		if got := renderToolResultPreview(act); got != "" {
			t.Errorf("renderToolResultPreview(%+v) = %q, want empty", act, got)
		}
	}
}

func TestRenderToolResultPreviewShowsShortResult(t *testing.T) {
	act := daemonclient.ToolActivity{Name: "bash", Result: "line one\nline two", OK: true}
	got := renderToolResultPreview(act)
	if !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("preview = %q, want both lines present", got)
	}
	if strings.Contains(got, "linhas") {
		t.Errorf("preview = %q, want no overflow suffix for a short result", got)
	}
}

func TestRenderToolResultPreviewTruncatesLongOutputWithLineCount(t *testing.T) {
	lines := make([]string, toolResultPreviewMaxLines+3)
	for i := range lines {
		lines[i] = "output line"
	}
	act := daemonclient.ToolActivity{Name: "bash", Result: strings.Join(lines, "\n")}
	got := renderToolResultPreview(act)
	if !strings.Contains(got, "+3 linhas") {
		t.Errorf("preview = %q, want a \"+3 linhas\" overflow suffix", got)
	}
	if strings.Count(got, "output line") != toolResultPreviewMaxLines {
		t.Errorf("preview shown lines = %d, want exactly %d", strings.Count(got, "output line"), toolResultPreviewMaxLines)
	}
}

func TestRenderToolResultPreviewTruncatesWideLine(t *testing.T) {
	act := daemonclient.ToolActivity{Name: "bash", Result: strings.Repeat("x", toolResultPreviewMaxWidth+50)}
	got := renderToolResultPreview(act)
	if !strings.Contains(got, "…") {
		t.Errorf("preview = %q, want an ellipsis marking the truncated line", got)
	}
	if strings.Count(got, "x") > toolResultPreviewMaxWidth {
		t.Errorf("preview kept %d raw characters, want at most %d", strings.Count(got, "x"), toolResultPreviewMaxWidth)
	}
}

func TestRenderToolResultPreviewStripsANSI(t *testing.T) {
	act := daemonclient.ToolActivity{Name: "bash", Result: "\x1b[31mred text\x1b[0m"}
	got := renderToolResultPreview(act)
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
