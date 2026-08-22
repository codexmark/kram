package app

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type textPoint struct{ x, y int }

type textSelection struct {
	active     bool
	moved      bool
	start, end textPoint
	lines      []string
}

type clipboardSequenceClearMsg struct{}
type copyNoticeClearMsg struct{ revision int }

func clearClipboardSequenceCmd() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return clipboardSequenceClearMsg{} })
}

func clearCopyNoticeCmd(revision int) tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return copyNoticeClearMsg{revision: revision} })
}

func beginTextSelection(view string, x, y int) textSelection {
	styled := strings.Split(view, "\n")
	lines := make([]string, len(styled))
	for i := range styled {
		lines[i] = ansi.Strip(styled[i])
	}
	p := textPoint{x: maxInt(0, x), y: clampInt(y, 0, maxInt(0, len(lines)-1))}
	return textSelection{active: true, start: p, end: p, lines: lines}
}

func (s *textSelection) move(x, y int) {
	s.end = textPoint{x: maxInt(0, x), y: clampInt(y, 0, maxInt(0, len(s.lines)-1))}
	s.moved = s.end != s.start
}

func (s textSelection) text() string {
	if len(s.lines) == 0 || !s.moved {
		return ""
	}
	start, end := s.start, s.end
	if start.y > end.y || (start.y == end.y && start.x > end.x) {
		start, end = end, start
	}
	var selected []string
	for row := start.y; row <= end.y && row < len(s.lines); row++ {
		line := s.lines[row]
		from, to := 0, ansi.StringWidth(line)
		if row == start.y {
			from = start.x
		}
		if row == end.y {
			to = end.x + 1
		}
		if to < from {
			to = from
		}
		selected = append(selected, ansi.Cut(line, from, to))
	}
	return strings.TrimSpace(strings.Join(selected, "\n"))
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
