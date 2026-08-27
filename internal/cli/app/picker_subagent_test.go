package app

import (
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

func pickerFixtureSessions() []daemonclient.Session {
	return []daemonclient.Session{
		{ID: "real1", Title: "fix the login bug"},
		{ID: "sub1", Title: "subagent: read every file under internal/daemon"},
		{ID: "real2", Title: ""},
		{ID: "sub2", Title: "subagent: run the test suite"},
	}
}

func TestPickerVisibleSessionsExcludesSubagentsByDefault(t *testing.T) {
	m := testModel(t)
	m.sessionList = pickerFixtureSessions()

	visible := m.pickerVisibleSessions()
	if len(visible) != 2 {
		t.Fatalf("default picker view = %d sessions, want 2 (subagent sessions excluded): %+v", len(visible), visible)
	}
	for _, sess := range visible {
		if isSubagentSessionTitle(sess.Title) {
			t.Fatalf("default picker view leaked a subagent session: %+v", sess)
		}
	}
}

func TestPickerSKeyTogglesToSubagentSessionsOnly(t *testing.T) {
	m := testModel(t)
	m.phase = phasePicker
	m.sessionList = pickerFixtureSessions()
	m.pickerCursor = 1

	next, cmd := m.handlePickerKey(keyMsg("s"))
	m = next.(Model)
	if cmd != nil {
		t.Fatal("toggling the picker view unexpectedly returned a command")
	}
	if !m.pickerShowSubagents {
		t.Fatal("\"s\" did not toggle pickerShowSubagents on")
	}
	if m.pickerCursor != 0 {
		t.Fatalf("cursor after toggle = %d, want reset to 0 (the two lists differ in length)", m.pickerCursor)
	}

	visible := m.pickerVisibleSessions()
	if len(visible) != 2 {
		t.Fatalf("subagent picker view = %d sessions, want 2: %+v", len(visible), visible)
	}
	for _, sess := range visible {
		if !isSubagentSessionTitle(sess.Title) {
			t.Fatalf("subagent picker view included a non-subagent session: %+v", sess)
		}
	}

	next, _ = m.handlePickerKey(keyMsg("s"))
	m = next.(Model)
	if m.pickerShowSubagents {
		t.Fatal("second \"s\" press did not toggle back to the default view")
	}
}

func TestPickerEnterOnSubagentSessionOpensItLikeAnyOther(t *testing.T) {
	m := testModel(t)
	m.phase = phasePicker
	m.sessionList = pickerFixtureSessions()
	m.pickerShowSubagents = true
	m.pickerCursor = 1 // first (only) subagent-view row, after the "new session" row

	next, cmd := m.handlePickerKey(keyMsg("enter"))
	m = next.(Model)
	if m.phase != phaseChat || cmd == nil {
		t.Fatalf("selecting a subagent session = phase %d cmd=%v, want phaseChat with a load command", m.phase, cmd != nil)
	}
	if !strings.HasPrefix(m.sessionID, "sub") {
		t.Fatalf("selected session id = %q, want one of the subagent fixtures", m.sessionID)
	}
}

func TestIsSubagentSessionTitle(t *testing.T) {
	cases := []struct {
		title string
		want  bool
	}{
		{"subagent: fix the bug", true},
		{"", false},
		{"a session about subagents", false},
	}
	for _, tc := range cases {
		if got := isSubagentSessionTitle(tc.title); got != tc.want {
			t.Errorf("isSubagentSessionTitle(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
}

func TestRenderPickerShowsSubagentHintAndModeHeader(t *testing.T) {
	m := testModel(t)
	m.phase = phasePicker
	m.sessionList = pickerFixtureSessions()

	defaultView := m.renderPicker()
	if !strings.Contains(defaultView, "2") || !strings.Contains(defaultView, "subagent") {
		t.Errorf("default picker view missing a hidden-subagent-count hint: %q", defaultView)
	}

	m.pickerShowSubagents = true
	subagentView := m.renderPicker()
	if !strings.Contains(subagentView, "subagent") {
		t.Errorf("subagent picker view missing its mode header: %q", subagentView)
	}
}
