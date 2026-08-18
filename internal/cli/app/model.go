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
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
	"github.com/codexmark/kram-gateway/internal/credentials"
	"github.com/codexmark/kram-gateway/internal/openai"
	"github.com/codexmark/kram-gateway/internal/toolsettings"
)

const footerHeight = 2 // the pulse bar is always exactly two lines

// phase tracks which screen the program is showing: the session picker
// (when launched without an explicit -session), the chat itself, or the
// accounts screen (add/remove provider API keys — reachable from the
// picker with "a").
type phase int

const (
	phasePicker phase = iota
	phaseChat
	phaseAccounts
	phaseTools
)

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
	// streaming is true from the moment this assistant message's turn
	// starts until its "done"/"error" event lands. While true, content
	// renders as plain text (deltas arriving mid-markdown would flicker
	// through half-parsed formatting); the full markdown render happens
	// once, when the message is complete.
	streaming bool
}

// Model is the CLI's full Bubble Tea state.
type Model struct {
	daemon    *daemonclient.Client
	gateway   *statusclient.Client
	combo     string // combo/model name the daemon sends messages to
	workspace string // project root, shown on the picker banner; "" if unknown

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
	// waitStartedAt/lastEventAt drive the rich status line (elapsed time)
	// and stalled-connection detection while a turn is in flight — real
	// signal, not just an animated glyph implying progress that may not
	// be happening. See the visual-research note in view.go's
	// thinkingLine for why this matters.
	waitStartedAt time.Time
	lastEventAt   time.Time

	width, height int
	ready         bool

	mdRenderer *glamour.TermRenderer
	mdWidth    int // width the current mdRenderer was built for

	phase          phase
	sessionList    []daemonclient.Session
	pickerCursor   int
	pickerErr      error
	pickerBusy     bool
	newSessionText textinput.Model // title prompt, only shown after picking "new session"
	titling        bool

	// accounts screen state — add/remove provider API keys without
	// leaving the CLI. credStore is nil only if the local credentials
	// file couldn't even be opened (rare; every screen guards for it).
	credStore            *credentials.Store
	accountsCursor       int
	accountsEditing      bool
	accountsKeyInput     textinput.Model
	accountsStatus       string
	accountsOAuthPending bool
	accountsOAuthURL     string

	// tools/skills toggle screen state.
	toolSettings *toolsettings.Store
	toolsList    []daemonclient.ToolInfo
	skillsList   []daemonclient.Skill
	toolsCursor  int
	toolsLoading bool
	toolsErr     error
	toolsStatus  string

	// question is set while an ask_question tool call is pausing the
	// current turn — see ask.go. Nil the rest of the time.
	question      *pendingQuestion
	questionInput textinput.Model

	err error
}

// New builds the initial model. If sessionID is empty, the program opens
// on the session picker instead of going straight to chat.
func New(daemon *daemonclient.Client, gateway *statusclient.Client, sessionID, combo, workspace string) Model {
	ti := textinput.New()
	ti.Placeholder = "mensagem…"
	ti.Focus()
	ti.CharLimit = 4000
	ti.Prompt = "› "

	titleInput := textinput.New()
	titleInput.Placeholder = "título (opcional, enter pra pular)"
	titleInput.CharLimit = 100
	titleInput.Prompt = "› "

	keyInput := textinput.New()
	keyInput.Placeholder = "sk-…"
	keyInput.CharLimit = 400
	keyInput.Prompt = "› "
	keyInput.EchoMode = textinput.EchoPassword
	keyInput.EchoCharacter = '•'

	answerInput := textinput.New()
	answerInput.Placeholder = "resposta…"
	answerInput.CharLimit = 2000
	answerInput.Prompt = "› "

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	credStore, _ := credentials.Load()     // nil on failure; every use site guards for it
	toolSettings, _ := toolsettings.Load() // same

	m := Model{
		daemon: daemon, gateway: gateway, combo: combo, workspace: workspace, sessionID: sessionID,
		input: ti, newSessionText: titleInput, accountsKeyInput: keyInput, questionInput: answerInput,
		viewport: viewport.New(80, 20), spin: sp,
		credStore: credStore, toolSettings: toolSettings,
	}
	if sessionID == "" {
		m.phase = phasePicker
		m.pickerBusy = true
	} else {
		m.phase = phaseChat
	}
	return m
}

// Init kicks off the session picker (if no session was given up front) or
// loads whatever history the daemon already has for an explicit session
// (it may not be empty — sessions are durable), plus a first, silent
// context-usage fetch so the footer icon has real data as soon as the
// screen draws.
func (m Model) Init() tea.Cmd {
	if m.phase == phasePicker {
		return tea.Batch(listSessionsCmd(m.daemon), m.spin.Tick)
	}
	return m.enterChatCmds()
}

func (m Model) enterChatCmds() tea.Cmd {
	return tea.Batch(textinput.Blink, loadHistoryCmd(m.daemon, m.sessionID), fetchContextCmd(m.daemon, m.sessionID))
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.syncViewportSize()
		if m.mdRenderer == nil || m.mdWidth != m.width {
			m.mdRenderer = newMarkdownRenderer(m.width)
			m.mdWidth = m.width
			m.refreshTranscript()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case sessionsListMsg:
		m.pickerBusy = false
		m.pickerErr = msg.err
		if msg.err == nil {
			m.sessionList = msg.sessions
		}
		return m, nil

	case sessionCreatedMsg:
		m.pickerBusy = false
		if msg.err != nil {
			m.pickerErr = msg.err
			return m, nil
		}
		m.sessionID = msg.session.ID
		m.phase = phaseChat
		m.syncViewportSize()
		return m, m.enterChatCmds()

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

	case streamStartMsg:
		if msg.err != nil {
			m.waiting = false
			m.err = msg.err
			m.dropStreamingPlaceholder()
			m.refreshTranscript()
			return m, nil
		}
		return m, readNextEventCmd(msg.stream)

	case streamEventMsg:
		return m.handleStreamEvent(msg)

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

	case oauthURLMsg:
		if msg.err != nil {
			m.accountsOAuthPending = false
			m.accountsStatus = "erro ao iniciar oauth: " + msg.err.Error()
			return m, nil
		}
		m.accountsOAuthURL = msg.url
		openBrowser(msg.url)
		return m, waitOpenRouterOAuthCmd(msg.wait)

	case oauthResultMsg:
		m.accountsOAuthPending = false
		if msg.err != nil {
			m.accountsStatus = "oauth falhou: " + msg.err.Error()
			return m, nil
		}
		if m.credStore != nil {
			if err := m.credStore.Set("OPENROUTER_API_KEY", msg.key); err != nil {
				m.accountsStatus = "erro ao salvar: " + err.Error()
			} else {
				m.accountsStatus = "OpenRouter conectado — reinicie o kram pra usar."
			}
		}
		return m, nil

	case toolsListMsg:
		m.toolsLoading = false
		m.toolsErr = msg.err
		if msg.err == nil {
			m.toolsList = msg.tools
			m.skillsList = msg.skills
		}
		return m, nil

	case answerSentMsg:
		if msg.err != nil {
			m.err = msg.err
			m.refreshTranscript()
		}
		return m, nil

	case animTickMsg:
		if !m.waiting {
			return m, nil
		}
		m.animFrame++
		m.refreshTranscript() // keeps the transcript's thinking line in step with the footer
		return m, animTickCmd()

	case spinner.TickMsg:
		if !m.waiting && !m.pickerBusy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.phase == phasePicker {
		return m.handlePickerKey(msg)
	}
	if m.phase == phaseAccounts {
		return m.handleAccountsKey(msg)
	}
	if m.phase == phaseTools {
		return m.handleToolsKey(msg)
	}
	if m.question != nil {
		return m.handleQuestionKey(msg)
	}

	switch msg.String() {
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

// handlePickerKey drives the session picker: an up-front list of existing
// sessions (most recently active first) plus a "new session" row, shown
// whenever the CLI is launched without an explicit -session.
func (m Model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.titling {
		switch msg.String() {
		case "esc":
			m.titling = false
			return m, nil
		case "enter":
			m.pickerBusy = true
			title := strings.TrimSpace(m.newSessionText.Value())
			return m, createSessionCmd(m.daemon, title)
		}
		var cmd tea.Cmd
		m.newSessionText, cmd = m.newSessionText.Update(msg)
		return m, cmd
	}

	itemCount := len(m.sessionList) + 1 // +1 for the "new session" row
	switch msg.String() {
	case "a":
		m.phase = phaseAccounts
		m.accountsStatus = ""
		return m, nil
	case "f":
		m.phase = phaseTools
		m.toolsStatus = ""
		m.toolsLoading = true
		return m, fetchToolsCmd(m.daemon)
	case "up":
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}
	case "down":
		if m.pickerCursor < itemCount-1 {
			m.pickerCursor++
		}
	case "enter":
		if m.pickerCursor == 0 {
			m.titling = true
			m.newSessionText.SetValue("")
			m.newSessionText.Focus()
			return m, nil
		}
		sess := m.sessionList[m.pickerCursor-1]
		m.sessionID = sess.ID
		m.phase = phaseChat
		m.syncViewportSize()
		return m, m.enterChatCmds()
	}
	return m, nil
}

// handleMouse implements the "discreet clickable icon" affordance: the
// context-usage badge on the footer's bottom-right is a real click target,
// not just a keyboard shortcut. The footer always occupies the terminal's
// last two rows, so hit-testing is just a column check against the same
// right-aligned block footerLine2 renders.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Enabling mouse mode at all (needed for the footer-icon click below)
	// takes the wheel away from the terminal's own scrollback, so we have
	// to implement it ourselves or the transcript becomes unscrollable by
	// mouse entirely. Text selection is unaffected by any of this — every
	// mainstream terminal (GNOME Terminal, kitty, Alacritty, foot, xterm)
	// lets you hold Shift while dragging to bypass an app's mouse capture
	// and select natively, same as it would for any other TUI.
	if msg.Button == tea.MouseButtonWheelUp {
		m.viewport.LineUp(3)
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelDown {
		m.viewport.LineDown(3)
		return m, nil
	}

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
	m.messages = append(m.messages,
		chatMessage{Role: "user", Content: text},
		chatMessage{Role: "assistant", streaming: true}, // filled in live as streamEventMsg events arrive
	)
	m.waiting = true
	m.animFrame = 0
	m.waitStartedAt = time.Now()
	m.lastEventAt = time.Now()
	m.err = nil
	m.refreshTranscript()
	return m, tea.Batch(startSendMessageCmd(m.daemon, m.sessionID, text), animTickCmd(), m.spin.Tick)
}

// dropStreamingPlaceholder removes the trailing empty assistant message
// submit() adds optimistically, for the case where the turn never
// actually started (e.g. the daemon was unreachable).
func (m *Model) dropStreamingPlaceholder() {
	if n := len(m.messages); n > 0 && m.messages[n-1].streaming {
		m.messages = m.messages[:n-1]
	}
}

// handleStreamEvent applies one event from an in-flight agent turn to the
// trailing (streaming) assistant message, and either continues reading
// the stream or, once it's done, settles the footer/context exactly like
// the old single-response flow did.
func (m Model) handleStreamEvent(msg streamEventMsg) (tea.Model, tea.Cmd) {
	m.lastEventAt = time.Now() // any event, including error, proves the connection is alive — resets stalled detection

	if msg.err != nil {
		m.waiting = false
		m.err = msg.err
		m.finishStreamingPlaceholder()
		m.refreshTranscript()
		return m, nil
	}

	n := len(m.messages)
	last := func() *chatMessage {
		if n == 0 {
			return nil
		}
		return &m.messages[n-1]
	}

	switch msg.event.Type {
	case "delta":
		if lm := last(); lm != nil {
			lm.Content += msg.event.Content
		}
		m.refreshTranscript()

	case "tool_start":
		if lm := last(); lm != nil {
			lm.ToolActivity = append(lm.ToolActivity, daemonclient.ToolActivity{
				Name: msg.event.Name, Args: msg.event.Args, Running: true,
			})
		}
		m.refreshTranscript()

	case "tool_result":
		if lm := last(); lm != nil {
			for i := len(lm.ToolActivity) - 1; i >= 0; i-- {
				if lm.ToolActivity[i].Name == msg.event.Name && lm.ToolActivity[i].Running {
					lm.ToolActivity[i].Result = msg.event.Result
					lm.ToolActivity[i].OK = msg.event.OK
					lm.ToolActivity[i].Running = false
					break
				}
			}
		}
		m.refreshTranscript()

	case "notice":
		if lm := last(); lm != nil {
			lm.Notices = append(lm.Notices, msg.event.Text)
		}
		m.refreshTranscript()

	case "question":
		// The turn is genuinely paused server-side until answered —
		// still read on from the stream below (readNextEventCmd), that
		// read just blocks harmlessly until AnswerQuestion unblocks the
		// daemon and it produces the next event.
		m.question = &pendingQuestion{id: msg.event.QuestionID, question: msg.event.Question, options: msg.event.Options}
		m.questionInput.SetValue("")
		m.questionInput.Focus()
		m.refreshTranscript()

	case "done":
		m.waiting = false
		m.err = nil
		m.lastProvider = msg.event.Message.Provider
		m.lastAttempts = msg.event.Attempts
		m.lastUsage = msg.event.Usage
		if lm := last(); lm != nil {
			lm.Content = msg.event.Message.Content // authoritative final text, in case deltas ever diverge
			lm.Provider = msg.event.Message.Provider
			lm.streaming = false
		}
		_ = msg.stream.Close()
		m.refreshTranscript()
		// The turn may have changed how much context is used (new
		// messages, maybe a compaction) — refresh the icon quietly.
		return m, fetchContextCmd(m.daemon, m.sessionID)

	case "error":
		m.waiting = false
		m.err = fmt.Errorf("%s", msg.event.Error)
		m.finishStreamingPlaceholder()
		_ = msg.stream.Close()
		m.refreshTranscript()
		return m, nil
	}

	if msg.done {
		// Reached EOF or a [DONE] marker without a recognized terminal
		// event — treat as a quiet end rather than reading forever.
		m.waiting = false
		m.finishStreamingPlaceholder()
		_ = msg.stream.Close()
		return m, nil
	}
	return m, readNextEventCmd(msg.stream)
}

// finishStreamingPlaceholder clears the streaming flag on the trailing
// assistant message without discarding whatever partial content it has —
// used when a stream ends abnormally, so a mid-answer failure still shows
// what was received instead of erasing it.
func (m *Model) finishStreamingPlaceholder() {
	if n := len(m.messages); n > 0 {
		m.messages[n-1].streaming = false
	}
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
