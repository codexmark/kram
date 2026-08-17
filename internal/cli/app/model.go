// Package app is the Kram CLI's Bubble Tea program: a chat transcript over
// a durable daemon session, with a compact live footer tracking the
// gateway's real fallback behavior and two on-demand panels — routing
// strategy and context-window usage. The CLI never talks to an LLM
// provider or persists anything itself — it's purely a view over what the
// daemon and gateway already own.
package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
	"github.com/codexmark/kram-gateway/internal/openai"
)

const footerHeight = 2 // the pulse bar is always exactly two lines

// panel identifies which (if any) of the two on-demand panels is open.
// Only one is ever shown at a time — the 25%-height budget is shared.
type panel int

const (
	panelNone panel = iota
	panelStrategy
	panelContext
)

type chatMessage struct {
	Role         string
	Content      string
	Provider     string
	ToolActivity []daemonclient.ToolActivity
	Notices      []string // e.g. image capability fallback, compaction happened
}

// Model is the CLI's full Bubble Tea state.
type Model struct {
	daemon  *daemonclient.Client
	gateway *statusclient.Client
	combo   string // combo/model name the daemon sends messages to

	sessionID string

	input    textinput.Model
	viewport viewport.Model
	spin     spinner.Model

	messages []chatMessage
	waiting  bool

	lastProvider string
	lastAttempts []openai.AttemptInfo
	lastUsage    openai.Usage

	active panel

	strategyData  statusclient.Status
	strategyErr   error
	strategyFocus int

	contextData daemonclient.ContextUsage
	contextErr  error
	haveContext bool

	animFrame int

	width, height int
	ready         bool

	err error
}

// New builds the initial model for a session that already exists in the daemon.
func New(daemon *daemonclient.Client, gateway *statusclient.Client, sessionID, combo string) Model {
	ti := textinput.New()
	ti.Placeholder = "mensagem…"
	ti.Focus()
	ti.CharLimit = 4000
	ti.Prompt = "› "

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return Model{
		daemon:    daemon,
		gateway:   gateway,
		combo:     combo,
		sessionID: sessionID,
		input:     ti,
		viewport:  viewport.New(80, 20),
		spin:      sp,
	}
}

// Init kicks off loading whatever history the daemon already has for this
// session (it may not be empty — sessions are durable) and a first,
// silent context-usage fetch so the footer icon has real data as soon as
// the screen draws.
func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, loadHistoryCmd(m.daemon, m.sessionID), fetchContextCmd(m.daemon, m.sessionID))
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.syncViewportSize()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case historyLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.messages = m.messages[:0]
		for _, hm := range msg.messages {
			m.messages = append(m.messages, chatMessage{Role: hm.Role, Content: hm.Content, Provider: hm.Provider})
		}
		m.refreshTranscript()
		return m, nil

	case sendResultMsg:
		m.waiting = false
		if msg.err != nil {
			m.err = msg.err
			m.refreshTranscript()
			return m, nil
		}
		m.err = nil
		m.lastProvider = msg.result.Message.Provider
		m.lastAttempts = msg.result.Attempts
		m.lastUsage = msg.result.Usage

		var notices []string
		if msg.result.ImageNotice != "" {
			notices = append(notices, msg.result.ImageNotice)
		}
		if msg.result.Compactions > 0 {
			notices = append(notices, fmt.Sprintf("session history was compacted %d time(s) to stay in budget", msg.result.Compactions))
		}

		m.messages = append(m.messages, chatMessage{
			Role: "assistant", Content: msg.result.Message.Content, Provider: msg.result.Message.Provider,
			ToolActivity: msg.result.ToolActivity, Notices: notices,
		})
		m.refreshTranscript()
		// The turn may have changed how much context is used (new
		// messages, maybe a compaction) — refresh the icon quietly.
		return m, fetchContextCmd(m.daemon, m.sessionID)

	case statusResultMsg:
		m.strategyErr = msg.err
		if msg.err == nil {
			m.strategyData = msg.status
		}
		return m, nil

	case contextResultMsg:
		m.contextErr = msg.err
		if msg.err == nil {
			m.contextData = msg.usage
			m.haveContext = true
		}
		return m, nil

	case animTickMsg:
		if !m.waiting {
			return m, nil
		}
		m.animFrame++
		return m, animTickCmd()

	case spinner.TickMsg:
		if !m.waiting {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit

	case "ctrl+p":
		return m.togglePanel(panelStrategy)

	case "ctrl+t":
		return m.togglePanel(panelContext)

	case "esc":
		if m.active != panelNone {
			m.active = panelNone
			m.syncViewportSize()
		}
		return m, nil

	case "up":
		if m.active == panelStrategy {
			if m.strategyFocus > 0 {
				m.strategyFocus--
			}
			return m, nil
		}
		m.viewport.LineUp(1)
		return m, nil

	case "down":
		if m.active == panelStrategy {
			combo := m.currentCombo()
			if combo != nil && m.strategyFocus < len(combo.Providers)-1 {
				m.strategyFocus++
			}
			return m, nil
		}
		m.viewport.LineDown(1)
		return m, nil

	case "enter":
		if m.active != panelNone {
			m.active = panelNone
			m.syncViewportSize()
			return m, nil
		}
		return m.submit()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleMouse implements the "discreet clickable icon" affordance: the
// context-usage badge on the footer's bottom-right is a real click target,
// not just a keyboard shortcut. The footer always occupies the terminal's
// last two rows, so hit-testing is just a column check against the same
// right-aligned block footerLine2 renders.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	footerRow2 := m.height - 1
	if msg.Y != footerRow2 {
		return m, nil
	}
	iconStart := m.width - lipgloss.Width(m.footerRightBlock())
	if msg.X >= iconStart {
		return m.togglePanel(panelContext)
	}
	return m, nil
}

func (m Model) togglePanel(p panel) (tea.Model, tea.Cmd) {
	if m.active == p {
		m.active = panelNone
		m.syncViewportSize()
		return m, nil
	}
	m.active = p
	m.syncViewportSize()
	switch p {
	case panelStrategy:
		m.strategyFocus = 0
		return m, fetchStatusCmd(m.gateway)
	case panelContext:
		return m, fetchContextCmd(m.daemon, m.sessionID)
	}
	return m, nil
}

func (m Model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" || m.waiting {
		return m, nil
	}
	m.input.SetValue("")
	m.messages = append(m.messages, chatMessage{Role: "user", Content: text})
	m.waiting = true
	m.animFrame = 0
	m.err = nil
	m.refreshTranscript()
	return m, tea.Batch(sendMessageCmd(m.daemon, m.sessionID, text), animTickCmd(), m.spin.Tick)
}

// currentCombo returns the combo the daemon is actually configured to use,
// falling back to the first combo the gateway reports if there's no exact
// match (e.g. status hasn't loaded yet).
func (m Model) currentCombo() *statusclient.Combo {
	for i := range m.strategyData.Combos {
		if m.strategyData.Combos[i].ID == m.combo {
			return &m.strategyData.Combos[i]
		}
	}
	if len(m.strategyData.Combos) > 0 {
		return &m.strategyData.Combos[0]
	}
	return nil
}

func (m *Model) syncViewportSize() {
	inputLines := 1
	reserved := footerHeight + inputLines
	if m.active != panelNone {
		reserved += m.panelHeight()
	}
	h := m.height - reserved
	if h < 3 {
		h = 3
	}
	m.viewport.Width = m.width
	m.viewport.Height = h
	m.input.Width = m.width - 2
}

func (m *Model) panelHeight() int {
	h := m.height / 4
	if h < 6 {
		h = 6
	}
	return h
}
