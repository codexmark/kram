package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

func TestBgProcessBadgeAbsentWithZeroProcesses(t *testing.T) {
	m := Model{}
	if got := m.bgProcessBadge(); got != "" {
		t.Fatalf("badge with zero processes = %q, want empty", got)
	}
}

func TestBgProcessBadgeShowsCountAndReflectsRunningState(t *testing.T) {
	m := Model{bgProcesses: []daemonclient.BackgroundProcess{
		processFixture("bg1", true), processFixture("bg2", false),
	}}
	got := m.bgProcessBadge()
	if got == "" {
		t.Fatal("badge with processes present = empty, want a rendered badge")
	}
	if !strings.Contains(got, "2 bg") {
		t.Fatalf("badge = %q, want it to mention the count (2 bg)", got)
	}

	allDone := Model{bgProcesses: []daemonclient.BackgroundProcess{processFixture("bg1", false)}}
	if got := allDone.bgProcessBadge(); got == "" {
		t.Fatal("badge with a finished-only process = empty, want a rendered (idle) badge")
	}
}

func TestBgBadgePollDoesNotFireWhilePanelIsOpen(t *testing.T) {
	m := Model{active: panelProcesses, bgBadgeGeneration: 3}
	_, cmd := m.Update(bgBadgePollTickMsg{generation: 3})
	if cmd != nil {
		t.Fatal("badge poll tick fired while the full panel was open — expected it to stay silent")
	}
}

func TestBgBadgePollIgnoresStaleGeneration(t *testing.T) {
	m := Model{active: panelNone, bgBadgeGeneration: 3}
	_, cmd := m.Update(bgBadgePollTickMsg{generation: 2})
	if cmd != nil {
		t.Fatal("badge poll tick fired for a stale generation — expected it to be dropped")
	}
}

func TestApplyBgProcessListUpdatesAndReschedulesWhenPanelClosed(t *testing.T) {
	m := Model{active: panelNone, bgBadgeGeneration: 1}
	processes := []daemonclient.BackgroundProcess{processFixture("bg1", true)}
	next, cmd := m.applyBgProcessList(bgProcessListMsg{generation: 1, processes: processes})
	m = next.(Model)
	if len(m.bgProcesses) != 1 {
		t.Fatalf("bgProcesses = %v, want 1 entry", m.bgProcesses)
	}
	if cmd == nil {
		t.Fatal("applyBgProcessList did not reschedule its own poll")
	}
}

func TestApplyBgProcessListDroppedWhilePanelOpen(t *testing.T) {
	m := Model{active: panelProcesses, bgBadgeGeneration: 1}
	next, cmd := m.applyBgProcessList(bgProcessListMsg{generation: 1, processes: []daemonclient.BackgroundProcess{processFixture("bg1", true)}})
	m = next.(Model)
	if len(m.bgProcesses) != 0 || cmd != nil {
		t.Fatal("a badge-poll response was applied even though the full panel had opened in the meantime")
	}
}

func TestClosingProcessPanelResumesBadgePollWithContinuity(t *testing.T) {
	m := New(nil, nil, "session", "default", t.TempDir(), false, WizardResult{BootSplashShown: true})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = sized.(Model)
	m.active = panelProcesses
	m.processes = []daemonclient.BackgroundProcess{processFixture("bg1", true)}
	m.bgBadgeGeneration = 1
	next, cmd := m.closeProcessPanel()
	m = next.(Model)
	if cmd == nil {
		t.Fatal("closing the process panel did not resume the badge poll")
	}
	if len(m.bgProcesses) != 1 {
		t.Fatal("closing the process panel did not seed the badge from the panel's own last-known processes")
	}
	if m.bgProcessBadge() == "" {
		t.Fatal("badge should render immediately on close, before the resumed poll's first response lands")
	}
}

func TestOpeningProcessPanelStopsBadgePoll(t *testing.T) {
	m := New(nil, nil, "session", "default", t.TempDir(), false, WizardResult{BootSplashShown: true})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = sized.(Model)
	m.bgBadgeGeneration = 1
	next, _ := m.openProcessPanel("")
	m = next.(Model)
	if _, cmd := m.Update(bgBadgePollTickMsg{generation: 1}); cmd != nil {
		t.Fatal("opening the process panel should invalidate the badge's pre-open poll generation")
	}
}

func newTestModelWithBadge(t *testing.T, processes []daemonclient.BackgroundProcess) Model {
	t.Helper()
	m := New(nil, nil, "session", "default", t.TempDir(), false, WizardResult{BootSplashShown: true})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(Model)
	m.bgProcesses = processes
	return m
}

func TestClickingBadgeOpensProcessPanelDirectly(t *testing.T) {
	m := newTestModelWithBadge(t, []daemonclient.BackgroundProcess{processFixture("bg1", true)})
	iconStart := m.width - lipgloss.Width(m.footerRightBlock())
	msg := tea.MouseMsg{X: iconStart, Y: m.height - 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
	next, cmd := m.handleMouse(msg)
	m = next.(Model)
	if m.active != panelProcesses || cmd == nil {
		t.Fatalf("clicking the badge did not open the process panel: active=%v cmd=%v", m.active, cmd)
	}
}

func TestClickingRestOfFooterRightBlockStillOpensContext(t *testing.T) {
	m := newTestModelWithBadge(t, []daemonclient.BackgroundProcess{processFixture("bg1", true)})
	iconStart := m.width - lipgloss.Width(m.footerRightBlock())
	badgeEnd := iconStart + lipgloss.Width(m.bgProcessBadge())
	msg := tea.MouseMsg{X: badgeEnd + 2, Y: m.height - 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
	next, cmd := m.handleMouse(msg)
	m = next.(Model)
	if m.active != panelContext || cmd == nil {
		t.Fatalf("clicking past the badge should still open the context panel: active=%v cmd=%v", m.active, cmd)
	}
}

func TestBgProcessListPollEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/processes" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]daemonclient.BackgroundProcess{processFixture("bg1", true)})
	}))
	defer srv.Close()

	client := daemonclient.New(srv.URL, "")
	m := New(client, nil, "session", "default", t.TempDir(), false, WizardResult{BootSplashShown: true})
	cmd := fetchProcessListCmd(client, m.bgBadgeGeneration)
	next, cmd := m.Update(cmd())
	m = next.(Model)
	if len(m.bgProcesses) != 1 || m.bgProcesses[0].ID != "bg1" {
		t.Fatalf("bgProcesses = %v, want [bg1]", m.bgProcesses)
	}
	if cmd == nil {
		t.Fatal("expected the badge poll to reschedule itself")
	}
}
