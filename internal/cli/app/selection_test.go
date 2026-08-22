package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRefreshTranscriptPreservesManualScrollAndFollowsBottom(t *testing.T) {
	m := New(nil, nil, "session", "default", t.TempDir(), false, WizardResult{BootSplashShown: true})
	m.ready = true
	m.width = 50
	m.height = 12
	m.syncViewportSize()
	for i := 0; i < 20; i++ {
		m.messages = append(m.messages, chatMessage{Role: "user", Content: strings.Repeat("line ", 8)})
	}
	m.refreshTranscript()
	if !m.viewport.AtBottom() {
		t.Fatal("initial transcript did not follow bottom")
	}
	m.viewport.SetYOffset(2)
	m.messages = append(m.messages, chatMessage{Role: "assistant", Content: "new output"})
	m.refreshTranscript()
	if m.viewport.YOffset != 2 {
		t.Fatalf("manual scroll moved to %d", m.viewport.YOffset)
	}
	m.viewport.GotoBottom()
	m.messages = append(m.messages, chatMessage{Role: "assistant", Content: "more output"})
	m.refreshTranscript()
	if !m.viewport.AtBottom() {
		t.Fatal("bottom-follow mode was not preserved")
	}
}

func TestMouseDragCopiesVisibleTextAndShowsFeedback(t *testing.T) {
	m := New(nil, nil, "session", "default", t.TempDir(), false, WizardResult{BootSplashShown: true})
	m.ready = true
	m.width = 60
	m.height = 15
	m.syncViewportSize()
	m.viewport.SetContent("alpha bravo\ncharlie delta")

	next, cmd := m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: routeBarHeight})
	m = next.(Model)
	if cmd != nil || !m.selection.active || m.copyNotice != "" {
		t.Fatalf("press state = active:%v notice:%q cmd:%v", m.selection.active, m.copyNotice, cmd)
	}
	next, _ = m.handleMouse(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: 4, Y: routeBarHeight})
	m = next.(Model)
	next, cmd = m.handleMouse(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonNone, X: 4, Y: routeBarHeight})
	m = next.(Model)
	if cmd == nil || m.selection.active || !strings.Contains(m.copyNotice, "✓ copiado") {
		t.Fatalf("release state = active:%v notice:%q cmd:%v", m.selection.active, m.copyNotice, cmd)
	}
	if !strings.Contains(m.clipboardSequence, "YWxwaGE=") { // base64("alpha") in OSC 52
		t.Fatalf("clipboard sequence = %q", m.clipboardSequence)
	}
	if !strings.HasPrefix(m.View(), m.clipboardSequence) {
		t.Fatal("clipboard sequence was not emitted in the rendered frame")
	}

	next, _ = m.Update(clipboardSequenceClearMsg{})
	m = next.(Model)
	if m.clipboardSequence != "" {
		t.Fatal("clipboard sequence did not clear")
	}
	next, _ = m.Update(copyNoticeClearMsg{revision: m.copyNoticeRevision})
	m = next.(Model)
	if m.copyNotice != "" {
		t.Fatal("copy notice did not clear")
	}
}

func TestTextSelectionAcrossLinesAndReverse(t *testing.T) {
	selection := beginTextSelection("first line\nsecond line", 6, 0)
	selection.move(5, 1)
	if got := selection.text(); got != "line\nsecond" {
		t.Fatalf("forward selection = %q", got)
	}
	reverse := beginTextSelection("first line\nsecond line", 5, 1)
	reverse.move(6, 0)
	if got := reverse.text(); got != "line\nsecond" {
		t.Fatalf("reverse selection = %q", got)
	}
}
