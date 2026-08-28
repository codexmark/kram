package app

import (
	"strings"
	"testing"
)

// knownAgentNotices mirrors the exact free-text strings the daemon actually
// emits via EventNotice (see internal/daemon/agent/agent.go and retry.go's
// EventNotice call sites) — a regression guard: if that text ever drifts,
// this test's expectations should be revisited deliberately rather than
// silently reclassifying a notice.
var knownAgentNotices = []struct {
	text        string
	wantWarning bool
}{
	{"session history was compacted to stay in budget", false},
	{`images were attached, but no provider in combo "gpt-5" supports image input — sent as text only`, false},
	{"provider attempted textual tool markup at the turn limit; Kram stopped it instead of exposing raw markup", true},
	{"provider returned textual tool markup; Kram normalized it and continued", false},
	{"stagnation detected in edit_file (3 identical failures)", true},
	{"transient gateway failure, retrying (round 2/3 in 400ms)", true},
	{"stream dropped mid-answer — resuming where it stopped (round 2/4 in 800ms)", true},
	{"provider rate limited — retrying in 34s (round 2/4)", true},
	{"summary model unavailable — oldest turns left out of this call's context (the session keeps them)", true},
	{"picked up 2 queued message(s) from you", false},
	{"verification gate: files changed but no build/test ran — asking the model to verify before finishing", false},
	{"checkpoint a1b2c3d saved (ctrl+g rewinds)", false},
}

func TestNoticeIsWarningClassifiesKnownDaemonNotices(t *testing.T) {
	for _, tc := range knownAgentNotices {
		if got := noticeIsWarning(tc.text); got != tc.wantWarning {
			t.Errorf("noticeIsWarning(%q) = %v, want %v", tc.text, got, tc.wantWarning)
		}
	}
}

func TestRenderNoticeUsesWarnGlyphOnlyForWarnings(t *testing.T) {
	info := renderNotice("session history was compacted to stay in budget")
	if strings.Contains(info, "⚠") {
		t.Errorf("informational notice rendered with the warning glyph: %q", info)
	}
	if !strings.Contains(info, "·") {
		t.Errorf("informational notice missing the plain bullet: %q", info)
	}

	warn := renderNotice("stagnation detected in edit_file (3 identical failures)")
	if !strings.Contains(warn, "⚠") {
		t.Errorf("warning notice missing the warning glyph: %q", warn)
	}
}

func TestRenderNoticePreservesTheFullText(t *testing.T) {
	text := "transient gateway failure, retrying (round 2/3 in 400ms)"
	if got := renderNotice(text); !strings.Contains(got, text) {
		t.Errorf("renderNotice(%q) = %q, lost the original text", text, got)
	}
}
