package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

func processFixture(id string, running bool) daemonclient.BackgroundProcess {
	return daemonclient.BackgroundProcess{
		ID: id, Command: "rails server", PID: 42, Running: running,
		StartedAt: time.Now().Add(-5 * time.Second), OutputBytes: 12, RetainedBytes: 12,
	}
}

func TestProcessPanelFetchesStructuredOutputAndPollsOnlyWhileOpen(t *testing.T) {
	var listCalls, outputCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/processes":
			listCalls++
			_ = json.NewEncoder(w).Encode([]daemonclient.BackgroundProcess{processFixture("bg1", true)})
		case "/processes/bg1/output":
			outputCalls++
			_ = json.NewEncoder(w).Encode(daemonclient.BackgroundProcessOutput{
				ID: "bg1", Output: "Puma ready\n", Cursor: 11, Reset: true, Running: true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := daemonclient.New(srv.URL)
	m := New(client, nil, "session", "default", t.TempDir(), false, WizardResult{BootSplashShown: true})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(Model)
	next, cmd := m.openProcessPanel("")
	m = next.(Model)
	if cmd == nil || m.active != panelProcesses {
		t.Fatal("opening the process panel did not start its initial snapshot")
	}
	next, poll := m.Update(cmd())
	m = next.(Model)
	if listCalls != 1 || outputCalls != 1 || poll == nil || m.processSelected != "bg1" {
		t.Fatalf("calls list=%d output=%d selected=%q poll=%v", listCalls, outputCalls, m.processSelected, poll != nil)
	}
	if got := m.processLogs["bg1"]; got != "Puma ready\n" {
		t.Fatalf("log = %q", got)
	}

	generation := m.processGeneration
	m.closeProcessPanel()
	next, cmd = m.Update(processPollTickMsg{generation: generation})
	m = next.(Model)
	if cmd != nil || m.active != panelNone {
		t.Fatal("a stale poll tick survived closing the observer")
	}
}

func TestProcessPanelCursorAppendResetAndLateResponse(t *testing.T) {
	m := testModel(t)
	m.active = panelProcesses
	m.processGeneration = 3
	m.processFollow = true
	process := processFixture("bg1", true)

	next, _ := m.applyProcessSnapshot(processSnapshotMsg{
		generation: 3, listed: true, processes: []daemonclient.BackgroundProcess{process}, selected: "bg1",
		output: &daemonclient.BackgroundProcessOutput{ID: "bg1", Output: "one", Cursor: 3, Reset: true},
	})
	m = next.(Model)
	next, _ = m.applyProcessSnapshot(processSnapshotMsg{
		generation: 3, listed: true, processes: []daemonclient.BackgroundProcess{process}, selected: "bg1",
		output: &daemonclient.BackgroundProcessOutput{ID: "bg1", Output: " two", Cursor: 7},
	})
	m = next.(Model)
	if m.processLogs["bg1"] != "one two" || m.processCursors["bg1"] != 7 {
		t.Fatalf("incremental state log=%q cursor=%d", m.processLogs["bg1"], m.processCursors["bg1"])
	}
	next, _ = m.applyProcessSnapshot(processSnapshotMsg{
		generation: 3, listed: true, processes: []daemonclient.BackgroundProcess{process}, selected: "bg1",
		output: &daemonclient.BackgroundProcessOutput{ID: "bg1", Output: "tail", Cursor: 20, Reset: true, Truncated: true},
	})
	m = next.(Model)
	if m.processLogs["bg1"] != "tail" || !m.processLogTruncated["bg1"] || !strings.Contains(ansi.Strip(m.processViewport.View()), "histórico anterior") {
		t.Fatalf("reset did not replace the local log: %q", m.processLogs["bg1"])
	}

	before := m.processLogs["bg1"]
	next, cmd := m.applyProcessSnapshot(processSnapshotMsg{
		generation: 2, output: &daemonclient.BackgroundProcessOutput{ID: "bg1", Output: "stale"},
	})
	m = next.(Model)
	if cmd != nil || m.processLogs["bg1"] != before {
		t.Fatal("late response from an older generation mutated the panel")
	}
}

func TestProcessPanelResponsiveTileTabAndSanitizedOutput(t *testing.T) {
	m := testModel(t)
	m.active = panelProcesses
	m.processes = []daemonclient.BackgroundProcess{processFixture("bg1", true)}
	m.processSelected = "bg1"
	m.processLogs["bg1"] = "\x1b[31mred\x1b[0m\rprogress\x00"
	m.syncViewportSize()
	m.syncTranscriptRenderer()
	m.refreshProcessViewportContent()
	if !m.processUsesTile() || m.viewport.Width >= m.width {
		t.Fatalf("wide layout did not reserve a side tile: viewport=%d total=%d", m.viewport.Width, m.width)
	}
	wide := ansi.Strip(m.View())
	if !strings.Contains(wide, "PROCESSOS") || !strings.Contains(wide, "red") || !strings.Contains(wide, "progress") || strings.Contains(wide, "\x1b[31m") {
		t.Fatalf("wide panel output was not rendered/sanitized: %q", wide)
	}

	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = next.(Model)
	if m.processUsesTile() || m.processPaneWidth() != 80 || m.viewport.Width != 80 {
		t.Fatalf("narrow layout did not become a full-width tab: pane=%d viewport=%d", m.processPaneWidth(), m.viewport.Width)
	}
	if !strings.Contains(ansi.Strip(m.View()), "PROCESSOS") {
		t.Fatal("narrow process tab disappeared")
	}
}

func TestClickingStructuredBackgroundActivityOpensExactProcess(t *testing.T) {
	m := testModel(t)
	m.messages = []chatMessage{{Role: "assistant", ToolActivity: []daemonclient.ToolActivity{{
		Name: "run_background", Args: `{"command":"rails server"}`, OK: true, ProcessID: "bg7",
	}}}}
	m.refreshTranscript()
	row := -1
	for candidate, id := range m.processLinkRows {
		if id == "bg7" {
			row = candidate
		}
	}
	if row < 0 {
		t.Fatal("rendered tool activity did not retain its structured process link")
	}
	y := routeBarHeight + row - m.viewport.YOffset
	next, _ := m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: y})
	m = next.(Model)
	next, cmd := m.handleMouse(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 2, Y: y})
	m = next.(Model)
	if m.active != panelProcesses || m.processSelected != "bg7" || cmd == nil {
		t.Fatalf("click opened panel=%d selected=%q cmd=%v", m.active, m.processSelected, cmd != nil)
	}
}

func TestProcessPanelKeyboardSelectionScrollAndClose(t *testing.T) {
	m := testModel(t)
	m.active = panelProcesses
	m.processes = []daemonclient.BackgroundProcess{processFixture("bg1", true), processFixture("bg2", false)}
	m.processSelected = "bg1"
	m.processLogs["bg1"] = strings.Repeat("line\n", 100)
	m.syncViewportSize()
	m.refreshProcessViewportContent()
	m.processViewport.GotoBottom()

	next, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(Model)
	if m.processSelected != "bg2" || cmd == nil {
		t.Fatalf("tab selected %q cmd=%v", m.processSelected, cmd != nil)
	}
	next, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.active != panelNone {
		t.Fatal("escape did not close process panel")
	}
}

func TestProcessPanelEmptyErrorExitedAndTruncatedStatesAreExplicit(t *testing.T) {
	m := testModel(t)
	m.active = panelProcesses
	m.processGeneration = 1
	m.processes = []daemonclient.BackgroundProcess{processFixture("old", true)}
	m.processSelected = "old"

	next, _ := m.applyProcessSnapshot(processSnapshotMsg{generation: 1, listed: true, processes: []daemonclient.BackgroundProcess{}})
	m = next.(Model)
	if len(m.processes) != 0 || m.processSelected != "" || !strings.Contains(ansi.Strip(m.renderProcessPane(20, 50)), "nenhum processo") {
		t.Fatalf("valid empty list state: processes=%v selected=%q pane=%q", m.processes, m.processSelected, ansi.Strip(m.renderProcessPane(20, 50)))
	}

	next, _ = m.applyProcessSnapshot(processSnapshotMsg{generation: 1, err: &testProcessError{"offline"}})
	m = next.(Model)
	if !strings.Contains(ansi.Strip(m.renderProcessPane(20, 50)), "daemon indisponível") {
		t.Fatal("list failure was rendered like a valid empty daemon")
	}

	ended := time.Now()
	failed := processFixture("bg9", false)
	failed.ExitCode = 2
	failed.ExitError = "exit status 2"
	failed.EndedAt = &ended
	failed.Truncated = true
	m.processErr = nil
	m.processes = []daemonclient.BackgroundProcess{failed}
	m.processSelected = "bg9"
	m.processLogs["bg9"] = "last line"
	m.syncProcessViewport()
	m.refreshProcessViewportContent()
	pane := ansi.Strip(m.renderProcessPane(20, 50))
	if !strings.Contains(pane, "encerrado 2") || !strings.Contains(pane, "exit sta") || !strings.Contains(pane, "histórico anterior") || !strings.Contains(pane, "last line") {
		t.Fatalf("failed/truncated process pane = %q", pane)
	}
}

type testProcessError struct{ text string }

func (e *testProcessError) Error() string { return e.text }

func TestProcessPanelPausedFollowCountsNewOutputAndEndResumes(t *testing.T) {
	m := testModel(t)
	m.active = panelProcesses
	m.processGeneration = 2
	m.processes = []daemonclient.BackgroundProcess{processFixture("bg1", true)}
	m.processSelected = "bg1"
	m.processLogs["bg1"] = strings.Repeat("old line\n", 80)
	m.syncProcessViewport()
	m.refreshProcessViewportContent()
	m.processViewport.GotoTop()
	m.processFollow = false
	cursor := int64(len(m.processLogs["bg1"]))
	m.processCursors["bg1"] = cursor
	m.processHaveCursor["bg1"] = true

	next, _ := m.applyProcessSnapshot(processSnapshotMsg{
		generation: 2, listed: true, processes: m.processes, selected: "bg1",
		output: &daemonclient.BackgroundProcessOutput{ID: "bg1", Output: "new bytes\n", Cursor: cursor + 10},
	})
	m = next.(Model)
	if m.processFollow || m.processNewBytes != 10 || m.processViewport.AtBottom() {
		t.Fatalf("paused follow state follow=%v new=%d bottom=%v", m.processFollow, m.processNewBytes, m.processViewport.AtBottom())
	}
	next, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
	m = next.(Model)
	if !m.processFollow || m.processNewBytes != 0 || !m.processViewport.AtBottom() {
		t.Fatal("End did not resume live tail following")
	}
	if cursor := m.processCursor(); cursor == nil || *cursor != int64(len(strings.Repeat("old line\n", 80)))+10 {
		t.Fatalf("process cursor = %v", cursor)
	}
}

func TestProcessPanelMouseTargetsItsOwnScrollAndRows(t *testing.T) {
	m := testModel(t)
	m.active = panelProcesses
	m.processes = []daemonclient.BackgroundProcess{processFixture("bg1", true), processFixture("bg2", true)}
	m.processSelected = "bg1"
	m.processLogs["bg1"] = strings.Repeat("line\n", 100)
	m.syncViewportSize()
	m.refreshProcessViewportContent()
	m.processViewport.GotoBottom()
	before := m.processViewport.YOffset
	x := m.viewport.Width + 2
	next, _ := m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp, X: x, Y: routeBarHeight + m.processOutputStartRow()})
	m = next.(Model)
	if m.processViewport.YOffset >= before || m.processFollow {
		t.Fatalf("process wheel did not scroll its own viewport: %d -> %d", before, m.processViewport.YOffset)
	}

	// The second visible process occupies body row 2 (title is row 0).
	next, cmd := m.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: routeBarHeight + 2})
	m = next.(Model)
	if m.processSelected != "bg2" || cmd == nil {
		t.Fatalf("row click selected=%q cmd=%v", m.processSelected, cmd != nil)
	}
}

func TestActivityLabelsCoverEveryObservableState(t *testing.T) {
	cases := []struct {
		state workState
		tool  string
		want  string
	}{
		{workPreparing, "", "PREPARANDO ROTA"},
		{workModelActive, "", "MODELO ATIVO"},
		{workToolActive, "bash", "EXECUTANDO · bash"},
		{workToolActive, "", "EXECUTANDO TOOL"},
		{workAnalyzingResult, "", "ANALISANDO RESULTADO"},
		{workWriting, "", "ESCREVENDO"},
	}
	for _, tc := range cases {
		m := Model{workState: tc.state, activeTool: tc.tool}
		if got := m.activityLabel(); got != tc.want {
			t.Errorf("state %d tool %q = %q, want %q", tc.state, tc.tool, got, tc.want)
		}
	}
}

func TestFormatProcessBytes(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		want  string
	}{{12, "12 B"}, {2048, "2.0 KB"}, {2 * 1024 * 1024, "2.0 MB"}} {
		if got := formatProcessBytes(tc.bytes); got != tc.want {
			t.Errorf("formatProcessBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
