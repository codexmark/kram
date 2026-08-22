package app

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/truncate"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

const (
	processTileWideMin  = 96
	processPollInterval = 750 * time.Millisecond
	processListMaxRows  = 4
	processLocalLogMax  = 500_000
)

type processSnapshotMsg struct {
	generation int
	processes  []daemonclient.BackgroundProcess
	listed     bool
	selected   string
	output     *daemonclient.BackgroundProcessOutput
	err        error
}

type processPollTickMsg struct{ generation int }

// fetchProcessSnapshotCmd deliberately polls only while the observer is open.
// It chooses the first process on an initial load server-side so list and
// output arrive atomically in one Bubble Tea message rather than flashing an
// empty detail pane for one poll interval.
func fetchProcessSnapshotCmd(c *daemonclient.Client, selected string, cursor *int64, generation int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		processes, err := c.ListBackgroundProcesses(ctx)
		if err != nil {
			return processSnapshotMsg{generation: generation, err: err}
		}
		if !containsProcess(processes, selected) {
			selected = ""
			cursor = nil
			if len(processes) > 0 {
				selected = processes[0].ID
			}
		}
		var output *daemonclient.BackgroundProcessOutput
		if selected != "" {
			got, outputErr := c.GetBackgroundProcessOutput(ctx, selected, cursor)
			if outputErr != nil {
				return processSnapshotMsg{
					generation: generation, processes: processes, listed: true,
					selected: selected, err: outputErr,
				}
			}
			output = &got
		}
		return processSnapshotMsg{
			generation: generation, processes: processes, listed: true,
			selected: selected, output: output,
		}
	}
}

func processPollTickCmd(generation int) tea.Cmd {
	return tea.Tick(processPollInterval, func(time.Time) tea.Msg {
		return processPollTickMsg{generation: generation}
	})
}

func containsProcess(processes []daemonclient.BackgroundProcess, id string) bool {
	for _, process := range processes {
		if process.ID == id {
			return true
		}
	}
	return false
}

func (m Model) processCursor() *int64 {
	if !m.processHaveCursor[m.processSelected] {
		return nil
	}
	cursor := m.processCursors[m.processSelected]
	return &cursor
}

func (m Model) applyProcessSnapshot(msg processSnapshotMsg) (tea.Model, tea.Cmd) {
	if m.active != panelProcesses || msg.generation != m.processGeneration {
		return m, nil // a response from a closed panel or prior selection
	}
	m.processLoading = false
	m.processErr = msg.err
	if msg.listed {
		m.processes = msg.processes
		if msg.selected == "" {
			m.processSelected = ""
		}
	}
	if msg.selected != "" {
		m.processSelected = msg.selected
	}
	if msg.err == nil && msg.output != nil {
		id := msg.output.ID
		chunk := msg.output.Output
		wasFollowing := m.processFollow && m.processViewport.AtBottom()
		if msg.output.Reset {
			m.processLogs[id] = chunk
			m.processLogTruncated[id] = msg.output.Truncated
		} else {
			m.processLogs[id] += chunk
			m.processLogTruncated[id] = m.processLogTruncated[id] || msg.output.Truncated
		}
		if len(m.processLogs[id]) > processLocalLogMax {
			m.processLogs[id] = m.processLogs[id][len(m.processLogs[id])-processLocalLogMax:]
		}
		m.processCursors[id] = msg.output.Cursor
		m.processHaveCursor[id] = true
		m.syncProcessViewport()
		m.refreshProcessViewportContent()
		if wasFollowing || m.processFollow {
			m.processFollow = true
			m.processNewBytes = 0
			m.processViewport.GotoBottom()
		} else if chunk != "" {
			m.processNewBytes += len(chunk)
		}
	} else {
		m.syncProcessViewport()
		m.refreshProcessViewportContent()
	}
	return m, processPollTickCmd(m.processGeneration)
}

func (m Model) openProcessPanel(id string) (tea.Model, tea.Cmd) {
	m.active = panelProcesses
	m.processGeneration++
	m.processSelected = id
	m.processLoading = true
	m.processErr = nil
	m.processFollow = true
	m.processNewBytes = 0
	m.syncViewportSize()
	m.syncTranscriptRenderer()
	return m, fetchProcessSnapshotCmd(m.daemon, id, m.processCursor(), m.processGeneration)
}

func (m *Model) closeProcessPanel() {
	m.active = panelNone
	m.processGeneration++ // invalidates an in-flight localhost request/tick
	m.selection.active = false
	m.syncViewportSize()
	m.syncTranscriptRenderer()
}

func (m Model) selectProcess(id string) (tea.Model, tea.Cmd) {
	if id == "" || id == m.processSelected {
		return m, nil
	}
	m.processGeneration++
	m.processSelected = id
	m.processErr = nil
	m.processLoading = true
	m.processFollow = true
	m.processNewBytes = 0
	m.syncProcessViewport()
	m.refreshProcessViewportContent()
	return m, fetchProcessSnapshotCmd(m.daemon, id, m.processCursor(), m.processGeneration)
}

func (m Model) selectAdjacentProcess(delta int) (tea.Model, tea.Cmd) {
	if len(m.processes) == 0 {
		return m, nil
	}
	index := 0
	for i := range m.processes {
		if m.processes[i].ID == m.processSelected {
			index = i
			break
		}
	}
	index = positiveModulo(index+delta, len(m.processes))
	return m.selectProcess(m.processes[index].ID)
}

func (m *Model) resumeProcessFollowIfAtBottom() {
	if m.processViewport.AtBottom() {
		m.processFollow = true
		m.processNewBytes = 0
	}
}

func (m Model) processUsesTile() bool {
	return m.active == panelProcesses && m.width >= processTileWideMin
}

func (m Model) processPaneWidth() int {
	if !m.processUsesTile() {
		return maxInt(1, m.width)
	}
	width := m.width * 42 / 100
	if width < 36 {
		width = 36
	}
	if width > 64 {
		width = 64
	}
	return width
}

func (m Model) chatViewportWidth() int {
	if !m.processUsesTile() {
		return maxInt(1, m.width)
	}
	return maxInt(32, m.width-m.processPaneWidth()-1)
}

func (m Model) visibleProcessIndices() []int {
	count := len(m.processes)
	if count == 0 {
		return nil
	}
	visible := minInt(processListMaxRows, count)
	selected := 0
	for i := range m.processes {
		if m.processes[i].ID == m.processSelected {
			selected = i
			break
		}
	}
	start := selected - visible/2
	if start < 0 {
		start = 0
	}
	if start+visible > count {
		start = count - visible
	}
	indices := make([]int, visible)
	for i := range indices {
		indices[i] = start + i
	}
	return indices
}

func (m *Model) syncProcessViewport() {
	indices := m.visibleProcessIndices()
	listRows := maxInt(1, len(indices))
	headerRows := listRows + 3                   // title + list/empty + separator + selected detail
	height := m.viewport.Height - headerRows - 1 // final hint row
	if height < 1 {
		height = 1
	}
	m.processViewport.Width = maxInt(1, m.processPaneWidth()-2)
	m.processViewport.Height = height
}

func (m *Model) refreshProcessViewportContent() {
	if m.processErr != nil {
		m.processViewport.SetContent(styleErrBadge.Render("não foi possível atualizar o processo:\n" + m.processErr.Error()))
		return
	}
	output := m.processLogs[m.processSelected]
	if output == "" {
		output = "(nenhuma saída produzida até agora)"
	}
	output = sanitizeProcessOutput(output)
	process := m.selectedProcess()
	if m.processLogTruncated[m.processSelected] || (process != nil && process.Truncated) {
		output = "[histórico anterior indisponível: o buffer/intervalo preservou somente a cauda]\n\n" + output
	}
	m.processViewport.SetContent(styleBody.Render(output))
}

func sanitizeProcessOutput(raw string) string {
	raw = ansi.Strip(raw)
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, raw)
}

func (m Model) selectedProcess() *daemonclient.BackgroundProcess {
	for i := range m.processes {
		if m.processes[i].ID == m.processSelected {
			return &m.processes[i]
		}
	}
	return nil
}

func (m Model) processOutputStartRow() int {
	return maxInt(1, len(m.visibleProcessIndices())) + 3
}

func (m Model) renderProcessPane(height, width int) string {
	if height < 1 || width < 1 {
		return ""
	}
	lines := []string{styleBadgeAccent.Bold(true).Render("PROCESSOS") + "  " + styleHint.Render("observação local · zero tokens")}
	indices := m.visibleProcessIndices()
	if len(indices) == 0 {
		status := "nenhum processo iniciado por run_background"
		if m.processLoading {
			status = "carregando processos…"
		} else if m.processErr != nil {
			status = "daemon indisponível: " + m.processErr.Error()
		}
		lines = append(lines, styleMeta.Render(status))
	} else {
		for _, index := range indices {
			process := m.processes[index]
			marker := "  "
			if process.ID == m.processSelected {
				marker = "▸ "
			}
			glyph := styleBadgeOK.Render("●")
			if !process.Running && process.ExitCode == 0 {
				glyph = styleBadgeIdle.Render("✓")
			} else if !process.Running {
				glyph = styleBadgeBad.Render("✗")
			}
			prefix := marker + glyph + " " + styleBadgeAccent.Render(process.ID) + " "
			commandWidth := maxInt(1, width-lipgloss.Width(prefix))
			command := truncate.StringWithTail(process.Command, uint(commandWidth), "…")
			lines = append(lines, prefix+styleMeta.Render(command))
		}
	}
	lines = append(lines, styleHint.Render(strings.Repeat("─", maxInt(1, width))))
	if process := m.selectedProcess(); process != nil {
		state := styleBadgeOK.Render("● rodando")
		if !process.Running && process.ExitCode == 0 {
			state = styleBadgeIdle.Render("✓ encerrado 0")
		} else if !process.Running {
			state = styleBadgeBad.Render(fmt.Sprintf("✗ encerrado %d", process.ExitCode))
		}
		elapsedEnd := time.Now()
		if process.EndedAt != nil {
			elapsedEnd = *process.EndedAt
		}
		elapsed := elapsedEnd.Sub(process.StartedAt).Round(time.Second)
		details := fmt.Sprintf("pid %d · %s · %s", process.PID, elapsed, formatProcessBytes(process.OutputBytes))
		if process.ExitError != "" {
			details += " · " + process.ExitError
		}
		prefix := styleBadgeAccent.Render(process.ID) + "  " + state + "  "
		detailWidth := maxInt(1, width-lipgloss.Width(prefix))
		details = truncate.StringWithTail(details, uint(detailWidth), "…")
		lines = append(lines, prefix+styleMeta.Render(details))
	} else if m.processErr != nil {
		lines = append(lines, styleErrBadge.Render("daemon: "+m.processErr.Error()))
	} else {
		lines = append(lines, styleMeta.Render("selecione um processo"))
	}

	lines = append(lines, m.processViewport.View())
	hint := "tab trocar · ↑↓ scroll · end seguir · esc fechar"
	if m.processNewBytes > 0 {
		hint = fmt.Sprintf("↓ %d bytes novos · end para acompanhar", m.processNewBytes)
	}
	lines = append(lines, styleHint.Render(hint))
	result := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Width(width).Height(height).MaxHeight(height).Render(result)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func formatProcessBytes(bytes int64) string {
	switch {
	case bytes >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	case bytes >= 1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
