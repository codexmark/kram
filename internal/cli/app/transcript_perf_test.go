package app

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// longSessionModel builds a realistic-scale session (40 prior turns, each
// with markdown content and tool activity, plus a still-streaming tail
// message) — the shape that made refreshTranscript's per-animFrame-tick
// cost visible in practice (issue #53's "travada" report) once the tick
// rate was raised for smoothness.
func longSessionModel(t testing.TB) Model {
	t.Helper()
	m := New(nil, nil, "s", "combo", t.TempDir(), false, WizardResult{BootSplashShown: true})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)

	for i := 0; i < 40; i++ {
		m.messages = append(m.messages,
			chatMessage{Role: "user", Content: strings.Repeat("pergunta do usuario ", 20)},
			chatMessage{Role: "assistant", Content: fmt.Sprintf("## Resposta %d\n\nTexto com **markdown** e `code`.\n\n- item um\n- item dois\n\n```go\nfunc x() {}\n```\n", i),
				ToolActivity: []daemonclient.ToolActivity{
					{Name: "read_file", Args: `{"path":"a.go"}`, Result: "conteudo\nlinhas\naqui", OK: true},
					{Name: "bash", Args: `{"command":"ls"}`, Result: "a\nb\nc", OK: true},
				}},
		)
	}
	m.messages = append(m.messages, chatMessage{Role: "assistant", streaming: true, Content: "parcial..."})
	m.waiting = true
	return m
}

// BenchmarkRefreshTranscriptLongSession is the cost animTickMsg used to
// pay on every single animation frame before refreshLiveIndicator
// existed — measured at ~16-18ms on this shape of session, a third of
// the 50ms tick budget on its own.
func BenchmarkRefreshTranscriptLongSession(b *testing.B) {
	m := longSessionModel(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.animFrame++
		m.refreshTranscript()
	}
}

// BenchmarkRefreshLiveIndicatorLongSession is what animTickMsg actually
// pays per frame now — re-rendering only the tail message instead of
// the whole transcript. Should be roughly an order of magnitude (or
// more) cheaper than BenchmarkRefreshTranscriptLongSession above on the
// same session shape; if a future change makes this regress back
// toward that cost, something has broken the static/tail split.
func BenchmarkRefreshLiveIndicatorLongSession(b *testing.B) {
	m := longSessionModel(b)
	m.refreshTranscript() // populate m.transcriptBody once, like a real full refresh would
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.animFrame++
		m.refreshLiveIndicator()
	}
}

// TestRefreshLiveIndicatorDoesNotRebuildStaticBody confirms the actual
// mechanism behind the speedup: repeated refreshLiveIndicator calls
// leave m.transcriptBody (the cached rendering of every message except
// the live tail) completely untouched — only refreshTranscript itself
// ever recomputes it.
func TestRefreshLiveIndicatorDoesNotRebuildStaticBody(t *testing.T) {
	m := longSessionModel(t)
	m.refreshTranscript()
	body := m.transcriptBody
	if body == "" {
		t.Fatal("test setup issue: transcriptBody is empty after a full refresh")
	}
	for i := 0; i < 5; i++ {
		m.animFrame++
		m.refreshLiveIndicator()
		if m.transcriptBody != body {
			t.Fatalf("transcriptBody changed after refreshLiveIndicator (frame %d) — it should only ever be touched by refreshTranscript", i)
		}
	}
}

// TestRefreshLiveIndicatorAnimatesRunningToolSpinner is the regression
// test for a real mistake caught before it shipped: an earlier version
// of this optimization cached everything except the trailing thinking
// line, which would have frozen a still-running tool call's spinner
// glyph (embedded mid-message, not just in the trailing suffix) instead
// of animating it. refreshLiveIndicator re-renders the whole tail
// message, spinner included, so this must never freeze.
func TestRefreshLiveIndicatorAnimatesRunningToolSpinner(t *testing.T) {
	m := testModel(t)
	m.waiting = true
	m.messages = []chatMessage{{
		Role: "assistant", streaming: true,
		ToolActivity: []daemonclient.ToolActivity{{Name: "bash", Args: "sleep 5", Running: true}},
	}}
	m.refreshTranscript()

	frames := make(map[string]bool)
	for i := 0; i < 8; i++ {
		m.animFrame++
		m.refreshLiveIndicator()
		frames[m.viewport.View()] = true
	}
	if len(frames) < 2 {
		t.Fatalf("running tool spinner never visibly changed across 8 animation frames — refreshLiveIndicator froze it")
	}
}

// TestStreamingEventsUseCheapTailPathNotFullRebuild is the regression
// test for #69: the four hot-path streaming events (delta, tool_start,
// tool_result, notice) mutate only the streaming tail message, so they
// must go through refreshLiveIndicator (cached static body) rather than
// refreshTranscript (full rebuild of every prior message per event).
// Proven by mutating m.transcriptBody to a sentinel after priming and
// asserting the events never overwrite it — only a full refresh would.
func TestStreamingEventsUseCheapTailPathNotFullRebuild(t *testing.T) {
	m := longSessionModel(t)
	m.refreshTranscript() // prime the cached static body (submit() does this in real use)

	const sentinel = "<<SENTINEL-BODY-DO-NOT-REBUILD>>"
	m.transcriptBody = sentinel

	events := []daemonclient.StreamEvent{
		{Type: "delta", Content: " more"},
		{Type: "tool_start", Name: "grep", Args: `{"pattern":"x"}`},
		{Type: "tool_result", Name: "grep", Result: "hit", OK: true},
		{Type: "notice", Text: "compaction happened"},
	}
	for _, ev := range events {
		next, _ := m.handleStreamEvent(streamEventMsg{event: ev})
		m = next.(Model)
		if m.transcriptBody != sentinel {
			t.Fatalf("event %q rebuilt the static transcript body — should have used the cheap tail path", ev.Type)
		}
	}

	// The tail content must still reflect the delta (cheap path must
	// actually render, not silently no-op).
	if !strings.Contains(m.viewport.View(), "more") {
		t.Errorf("streaming delta content did not reach the rendered viewport: %q", m.viewport.View())
	}
}
