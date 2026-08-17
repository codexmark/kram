// Package app is the Kram CLI's Bubble Tea program: a chat transcript over
// a durable daemon session, with a compact live footer tracking the
// gateway's real fallback behavior and an on-demand panel showing the
// full routing strategy. The CLI never talks to an LLM provider or
// persists anything itself — it's purely a view over what the daemon and
// gateway already own.
package app

import (
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
	"github.com/codexmark/kram-gateway/internal/openai"
)

const footerHeight = 2 // the pulse bar is always exactly two lines

type chatMessage struct {
	Role     string
	Content  string
	Provider string
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

	panelOpen  bool
	panelData  statusclient.Status
	panelErr   error
	panelFocus int

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
// session (it may not be empty — sessions are durable).
func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, loadHistoryCmd(m.daemon, m.sessionID))
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
		m.messages = append(m.messages, chatMessage{
			Role: "assistant", Content: msg.result.Message.Content, Provider: msg.result.Message.Provider,
		})
		m.refreshTranscript()
		return m, nil

	case statusResultMsg:
		m.panelErr = msg.err
		if msg.err == nil {
			m.panelData = msg.status
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
		m.panelOpen = !m.panelOpen
		m.syncViewportSize()
		if m.panelOpen {
			m.panelFocus = 0
			return m, fetchStatusCmd(m.gateway)
		}
		return m, nil

	case "esc":
		if m.panelOpen {
			m.panelOpen = false
			m.syncViewportSize()
		}
		return m, nil

	case "up":
		if m.panelOpen {
			if m.panelFocus > 0 {
				m.panelFocus--
			}
			return m, nil
		}
		m.viewport.LineUp(1)
		return m, nil

	case "down":
		if m.panelOpen {
			combo := m.currentCombo()
			if combo != nil && m.panelFocus < len(combo.Providers)-1 {
				m.panelFocus++
			}
			return m, nil
		}
		m.viewport.LineDown(1)
		return m, nil

	case "enter":
		if m.panelOpen {
			m.panelOpen = false
			m.syncViewportSize()
			return m, nil
		}
		return m.submit()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
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
	for i := range m.panelData.Combos {
		if m.panelData.Combos[i].ID == m.combo {
			return &m.panelData.Combos[i]
		}
	}
	if len(m.panelData.Combos) > 0 {
		return &m.panelData.Combos[0]
	}
	return nil
}

func (m *Model) syncViewportSize() {
	inputLines := 1
	reserved := footerHeight + inputLines
	if m.panelOpen {
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
