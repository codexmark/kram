package app

import (
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

func TestHandleStreamEventReasoningSetsPreviewWithoutTouchingMessageContent(t *testing.T) {
	m := testModel(t)
	m.messages = []chatMessage{{Role: "assistant", streaming: true}}
	m.workState = workModelActive

	next, _ := m.handleStreamEvent(streamEventMsg{event: daemonclient.StreamEvent{Type: "reasoning", Content: "weighing it up"}})
	m = next.(Model)

	if m.reasoningPreview != "weighing it up" {
		t.Errorf("reasoningPreview = %q, want %q", m.reasoningPreview, "weighing it up")
	}
	if m.messages[0].Content != "" {
		t.Errorf("reasoning event leaked into the assistant message content: %q", m.messages[0].Content)
	}
}

func TestHandleStreamEventDeltaClearsReasoningPreview(t *testing.T) {
	m := testModel(t)
	m.messages = []chatMessage{{Role: "assistant", streaming: true}}
	m.reasoningPreview = "still thinking"

	next, _ := m.handleStreamEvent(streamEventMsg{event: daemonclient.StreamEvent{Type: "delta", Content: "the answer"}})
	m = next.(Model)

	if m.reasoningPreview != "" {
		t.Errorf("reasoningPreview = %q, want cleared once real content starts arriving", m.reasoningPreview)
	}
	if m.messages[0].Content != "the answer" {
		t.Errorf("assistant content = %q, want %q", m.messages[0].Content, "the answer")
	}
}

func TestHandleStreamEventToolStartClearsReasoningPreview(t *testing.T) {
	m := testModel(t)
	m.messages = []chatMessage{{Role: "assistant", streaming: true}}
	m.reasoningPreview = "still thinking"

	next, _ := m.handleStreamEvent(streamEventMsg{event: daemonclient.StreamEvent{Type: "tool_start", Name: "bash", Args: "ls"}})
	m = next.(Model)

	if m.reasoningPreview != "" {
		t.Errorf("reasoningPreview = %q, want cleared once a tool call starts", m.reasoningPreview)
	}
}

func TestBoundedReasoningPreviewTruncatesLongText(t *testing.T) {
	short := "a brief thought"
	if got := boundedReasoningPreview(short); got != short {
		t.Errorf("boundedReasoningPreview(%q) = %q, want unchanged", short, got)
	}

	long := strings.Repeat("word ", 30)
	got := boundedReasoningPreview(long)
	if len([]rune(got)) > reasoningPreviewMaxRunes+1 { // +1 for the trailing "…"
		t.Errorf("boundedReasoningPreview truncated result too long: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("boundedReasoningPreview(long) = %q, want an ellipsis suffix", got)
	}
}

func TestThinkingLineShowsReasoningPreviewOnlyWhileModelActive(t *testing.T) {
	now := time.Now()
	active := Model{
		waitStartedAt: now.Add(-2 * time.Second), lastEventAt: now, animFrame: 1,
		workState: workModelActive, reasoningPreview: "considering options",
	}
	if got := active.thinkingLine(); !strings.Contains(got, "thinking: considering options") {
		t.Errorf("thinkingLine with an active reasoning preview = %q, want it to mention the preview", got)
	}

	writing := active
	writing.workState = workWriting
	if got := writing.thinkingLine(); strings.Contains(got, "thinking:") {
		t.Errorf("thinkingLine while workWriting = %q, want no reasoning preview shown once past workModelActive", got)
	}

	noPreview := active
	noPreview.reasoningPreview = ""
	if got := noPreview.thinkingLine(); strings.Contains(got, "thinking:") {
		t.Errorf("thinkingLine with an empty preview = %q, want no \"thinking:\" segment at all", got)
	}
}
