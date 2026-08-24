// Package app is the Kram CLI's Bubble Tea program: a chat transcript over
// a durable daemon session, with a compact live footer tracking the
// gateway's real fallback behavior and two on-demand panels — routing
// strategy and context-window usage. The CLI never talks to an LLM
// provider or persists anything itself — it's purely a view over what the
// daemon and gateway already own.
package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/cli/statusclient"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/providerping"
	"github.com/codexmark/kram/internal/toolsettings"
)

const footerHeight = 1   // context/tokens/shortcuts, one line — see view.go's renderFooter
const routeBarHeight = 1 // the route bar is always exactly one line
// inputHeight is the composer's fixed visible height in rows. A textarea
// (not textinput) word-wraps a long message across these rows instead of
// scrolling it off-screen horizontally — the specific bug this fixes.
// Fixed rather than dynamically growing with content: simpler layout math
// (syncViewportSize never has to react to how much the user has typed),
// and 3 rows already comfortably fits several sentences before the
// textarea's own internal scrolling takes over.
const inputHeight = 3

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
	// Stage 1 (pre-daemon, standalone wizard program) — see wizard.go.
	// phaseAccounts is reused for the provider step (wizardMode gates its
	// differing copy/keybindings) rather than adding a third phase here.
	phaseWizardEnvironment
	phaseWizardProjects
	phaseWizardRouting
	phaseWizardPermissions
	// Stage 2 (post-daemon, normal program) — reached only right after a
	// successful wizard run. phaseTools is reused for the "custom" branch
	// of the tools/skills preset step.
	phaseWizardToolsPreset
	phaseWizardSystemCheck
	phaseWizardSummary
	phaseSplash
)

// panel identifies which (if any) of the two on-demand panels is open.
// Only one is ever shown at a time — the 25%-height budget is shared.
type panel int

const (
	panelNone panel = iota
	panelStrategy
	panelStrategyPicker
	panelContext
	panelRoute
	panelProcesses
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

// workState is driven exclusively by daemon events already produced for the
// current run. It gives the animation honest, token-free vocabulary without
// asking the model to narrate intentions it may not actually follow.
type workState int

const (
	workPreparing workState = iota
	workModelActive
	workToolActive
	workAnalyzingResult
	workWriting
)

// Model is the CLI's full Bubble Tea state.
type Model struct {
	daemon    *daemonclient.Client
	gateway   *statusclient.Client
	combo     string // combo/model name the daemon sends messages to
	workspace string // project root enforced by the daemon; "" if unknown

	sessionID string

	input    textarea.Model // multi-line, word-wraps long messages instead of scrolling off-screen — see inputHeight
	viewport viewport.Model
	// processViewport scrolls independently from the transcript when the
	// Ctrl+B observer is open. The daemon remains the source of truth; this
	// viewport only owns the currently retained presentation tail.
	processViewport viewport.Model
	spin            spinner.Model

	messages []chatMessage
	waiting  bool

	lastUsage openai.Usage

	// Route bar state — see routebar.go. routeRunning is true between a
	// route_start event and its matching route_done (or the turn ending
	// without one, e.g. an error before any model call went out).
	// routeCall is the most recently *completed* model call's routing
	// story for the current turn; true per-attempt progress isn't
	// observable while routeRunning (see DECISIONS.md), so the bar animates
	// the real candidate count until the real trail lands.
	routeRunning bool
	routeCall    *daemonclient.RouteCall
	// routeTrace is the full routing story for the most recently
	// completed turn — every model call, not just the last one (see
	// agent.RouteTrace) — populated from the "done" event, rendered by
	// the Ctrl+R panel (routepanel.go).
	routeTrace daemonclient.RouteTrace

	active panel

	strategyData        statusclient.Status
	strategyErr         error
	strategyFocus       int
	strategyPickerFocus int
	strategySwitching   bool
	strategyPickerErr   error
	strategyNotice      string
	strategyNoticeRev   int

	contextData daemonclient.ContextUsage
	contextErr  error
	haveContext bool

	processes           []daemonclient.BackgroundProcess
	processSelected     string
	processErr          error
	processLoading      bool
	processGeneration   int
	processLogs         map[string]string
	processCursors      map[string]int64
	processHaveCursor   map[string]bool
	processLogTruncated map[string]bool
	processFollow       bool
	processNewBytes     int
	processLinkRows     map[int]string // absolute transcript row -> background process ID
	selectionInProcess  bool

	animFrame int
	// Mouse selection belongs to the app because enabling terminal mouse
	// tracking (needed for scrolling and clickable panels) disables native
	// drag selection. We snapshot the visible, ANSI-free viewport on press,
	// copy the selected cells through OSC 52 on release, and leave a short
	// confirmation in the footer.
	selection          textSelection
	clipboardSequence  string
	copyNotice         string
	copyNoticeRevision int
	// waitStartedAt/lastEventAt drive the rich status line (elapsed time)
	// and stalled-connection detection while a turn is in flight — real
	// signal, not just an animated glyph implying progress that may not
	// be happening. See the visual-research note in view.go's
	// thinkingLine for why this matters.
	waitStartedAt time.Time
	lastEventAt   time.Time
	workState     workState
	activeTool    string
	toolStartedAt time.Time
	heartbeats    int
	segment       int
	segments      int

	width, height int
	ready         bool

	mdRenderer *glamour.TermRenderer
	mdWidth    int // width the current mdRenderer was built for

	phase          phase
	splashTarget   phase // screen revealed after the boot animation completes
	splashFrame    int
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
	// accountsOAuthCancel stops the pending flow's callback listener the
	// instant the user backs out (esc/ctrl+c) instead of leaving it
	// running detached for up to callbackTimeout — set when the OAuth
	// wait begins (oauthURLMsg), cleared once it resolves either way.
	accountsOAuthCancel context.CancelFunc
	// accountsPings holds the most recent real connectivity/auth check per
	// account (keyed by EnvVar — see providerping.Ping), and
	// accountsPinging is true while a batch is in flight. Never simulated:
	// a row with no entry here has simply never been checked yet.
	accountsPings   map[string]providerping.Result
	accountsPinging bool

	// customStore is the user-registered custom-provider list (URL +
	// optional key/model, for a local/LAN OpenAI-compatible server — see
	// internal/customprovider), nil only if the file couldn't be opened.
	// customProviders is a cache of customStore.All() refreshed after
	// every add/delete, rendered as extra rows below the static catalog
	// in the accounts screen. accountsAddingCustom/customFormInputs/
	// customFormCursor drive the "+ add custom provider" form.
	customStore          *customprovider.Store
	customProviders      []customprovider.Provider
	accountsAddingCustom bool
	customFormInputs     []textinput.Model
	customFormCursor     int

	// tools/skills toggle screen state.
	toolSettings *toolsettings.Store
	toolsList    []daemonclient.ToolInfo
	skillsList   []daemonclient.Skill
	toolsCursor  int
	toolsLoading bool
	toolsErr     error
	toolsStatus  string

	// wizard (first-run setup) state — see wizard.go. wizardMode is true
	// only for the Stage-1 standalone program RunWizard runs before the
	// daemon/gateway exist; the Stage-2 continuation (tools preset/system
	// check/summary) runs in the normal post-daemon program instead, with
	// wizardMode left false, since it needs a live daemon connection.
	wizardMode                    bool
	wizardWorkspaceLocked         bool // true when -workspace was passed explicitly; skips the workspace question
	wizardStep                    int  // 1..8, drives the shared "N/8 Title" header
	wizardCWD                     string
	wizardHasGit                  bool
	wizardHomeDir                 string
	wizardProjectsRootInput       textinput.Model
	wizardWorkspaceInput          textinput.Model
	wizardProjectsField           int // 0 = editing projects root, 1 = editing workspace
	wizardWorkspaceErr            error
	wizardChosenWorkspace         string
	wizardProjectsRoot            string
	wizardRoutingCursor           int
	wizardChosenStrategy          string // "" (Auto), "smart", "round-robin"
	wizardPermCursor              int
	wizardChosenPermPreset        string // "recommended", "strict", "autonomous"
	wizardDone                    bool
	wizardCancelled               bool
	wizardProviderOverrideVisible bool

	// Stage-2 continuation (post-daemon) — reached only right after a
	// successful Stage-1 wizard run.
	wizardToolsPresetCursor   int
	wizardToolSettingsPending bool
	wizardCompletionErr       error
	wizardChosenToolsPreset   string // "recommended", "minimal", "custom"
	// wizardWelcomeSession is set right before creating the wizard's
	// final session, so the very first (empty) transcript render can show
	// a one-time, client-side-only welcome banner — never persisted as a
	// message, never mistaken for a real model reply (see wizard.go).
	wizardWelcomeSession bool

	// question is set while an ask_question tool call is pausing the
	// current turn — see ask.go. Nil the rest of the time.
	question      *pendingQuestion
	questionInput textinput.Model

	// approval is set while a permission-policy Ask decision is pausing
	// the current turn — see approval.go. Nil the rest of the time.
	// Mutually exclusive with question in practice (a turn only ever
	// blocks on one thing at once), but tracked separately since they're
	// semantically different pauses.
	approval *pendingApproval

	err error
}

// New builds the initial model. If sessionID is empty, the program opens
// on the session picker instead of going straight to chat — unless
// openOnToolsPreset is true, meaning the first-run wizard just completed
// successfully in a separate program moments ago (see wizard.go,
// cmd/kram/main.go), in which case it opens on the wizard's Stage-2
// continuation (tools/skills preset) instead, so the setup flow feels
// continuous instead of dropping into an empty picker. wizard carries
// Stage 1's choices purely for Stage-2's summary screen to display —
// this Model is a separate process/program from Stage 1's, so it has no
// other way to know what an earlier program's user picked; ignored
// (pass a zero WizardResult) when openOnToolsPreset is false.
func New(daemon *daemonclient.Client, gateway *statusclient.Client, sessionID, combo, workspace string, openOnToolsPreset bool, wizard WizardResult) Model {
	ti := textarea.New()
	ti.Placeholder = "mensagem…"
	ti.CharLimit = 4000
	ti.ShowLineNumbers = false
	// "› " only on the composer's first visual line, matching the old
	// single-line prompt; wrapped continuation lines get blank padding of
	// the same width instead of repeating the arrow on every line.
	ti.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return "› "
		}
		return "  "
	})
	ti.SetHeight(inputHeight)
	ti.Focus()

	titleInput := textinput.New()
	titleInput.Placeholder = "título (opcional, enter pra pular)"
	titleInput.CharLimit = 100
	titleInput.Prompt = "› "

	keyInput := newAccountsKeyInput()

	answerInput := textinput.New()
	answerInput.Placeholder = "resposta…"
	answerInput.CharLimit = 2000
	answerInput.Prompt = "› "

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	credStore, _ := credentials.Load()     // nil on failure; every use site guards for it
	toolSettings, _ := toolsettings.Load() // same
	customStore, _ := customprovider.Load()
	var customProviders []customprovider.Provider
	if customStore != nil {
		customProviders = customStore.All()
	}

	m := Model{
		daemon: daemon, gateway: gateway, combo: combo, workspace: workspace, sessionID: sessionID,
		input: ti, newSessionText: titleInput, accountsKeyInput: keyInput, questionInput: answerInput,
		viewport: viewport.New(80, 20), processViewport: viewport.New(36, 20), spin: sp,
		credStore: credStore, toolSettings: toolSettings,
		customStore: customStore, customProviders: customProviders,
		processLogs: make(map[string]string), processCursors: make(map[string]int64),
		processHaveCursor: make(map[string]bool), processLogTruncated: make(map[string]bool), processFollow: true,
		processLinkRows: make(map[int]string),
	}
	target := phasePicker
	switch {
	case sessionID != "":
		target = phaseChat
	case openOnToolsPreset:
		target = phaseWizardToolsPreset
		m.wizardStep = 6
		m.toolsLoading = true
		m.wizardChosenStrategy = wizard.Strategy
		m.wizardChosenPermPreset = wizard.PermPreset
		m.wizardProjectsRoot = wizard.ProjectsRoot
	default:
		target = phasePicker
		m.pickerBusy = true
	}
	if wizard.BootSplashShown {
		m.phase = target
	} else {
		m.phase = phaseSplash
		m.splashTarget = target
	}
	return m
}

// Init kicks off the session picker (if no session was given up front) or
// loads whatever history the daemon already has for an explicit session
// (it may not be empty — sessions are durable), plus a first, silent
// context-usage fetch so the footer icon has real data as soon as the
// screen draws.
func (m Model) Init() tea.Cmd {
	if m.phase == phaseSplash {
		return splashTickCmd()
	}
	return m.phaseInitCmd(m.phase)
}

func (m Model) phaseInitCmd(p phase) tea.Cmd {
	switch p {
	case phasePicker:
		return tea.Batch(listSessionsCmd(m.daemon), m.spin.Tick)
	case phaseWizardToolsPreset:
		return fetchToolsCmd(m.daemon)
	case phaseWizardEnvironment:
		// Stage 1 of the first-run wizard runs in its own standalone
		// program before the daemon/gateway exist — nothing here can
		// query them yet (see wizard.go, RunWizard).
		return textinput.Blink
	case phaseChat:
		return m.enterChatCmds()
	}
	return nil
}

func (m Model) enterChatCmds() tea.Cmd {
	// fetchStatusCmd is prefetched here (not only on-demand when Ctrl+P
	// opens) so the route bar has a real strategy name/combo to show from
	// the very first turn, same reasoning as the context-usage prefetch.
	return tea.Batch(
		textinput.Blink, loadHistoryCmd(m.daemon, m.sessionID),
		fetchContextCmd(m.daemon, m.sessionID), fetchStatusCmd(m.gateway),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		if m.wizardMode {
			// Stage 1's standalone program never renders the chat
			// viewport/composer/markdown — syncViewportSize would size a
			// textarea that newWizardModel deliberately never
			// textarea.New()'d (wizard mode has no message composer),
			// which panics on a zero-value textarea.Model.
			return m, nil
		}
		m.syncViewportSize()
		m.syncTranscriptRenderer()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case splashTickMsg:
		if m.phase != phaseSplash {
			return m, nil
		}
		if !m.ready {
			return m, splashTickCmd()
		}
		m.splashFrame++
		if m.splashFrame >= splashTotalFrames {
			return m.finishSplash()
		}
		return m, splashTickCmd()

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
			if m.active == panelStrategyPicker {
				m.syncStrategyPickerFocus()
			}
		}
		return m, nil

	case strategySetMsg:
		m.strategySwitching = false
		if msg.err != nil {
			m.strategyPickerErr = msg.err
			return m, nil
		}
		for i := range m.strategyData.Combos {
			if m.strategyData.Combos[i].ID == msg.combo.ID {
				m.strategyData.Combos[i] = msg.combo
			}
		}
		m.routeCall = nil // an old completed call must not overwrite the new label
		m.strategyPickerErr = nil
		m.strategyNoticeRev++
		m.strategyNotice = "✓ estratégia: " + strings.ToUpper(msg.combo.Strategy)
		m.active = panelNone
		m.syncViewportSize()
		m.syncTranscriptRenderer()
		return m, clearStrategyNoticeCmd(m.strategyNoticeRev)

	case strategyNoticeClearMsg:
		if msg.revision == m.strategyNoticeRev {
			m.strategyNotice = ""
		}
		return m, nil

	case contextResultMsg:
		m.contextErr = msg.err
		if msg.err == nil {
			m.contextData = msg.usage
			m.haveContext = true
		}
		return m, nil

	case processSnapshotMsg:
		return m.applyProcessSnapshot(msg)

	case processPollTickMsg:
		if m.active != panelProcesses || msg.generation != m.processGeneration {
			return m, nil
		}
		return m, fetchProcessSnapshotCmd(m.daemon, m.processSelected, m.processCursor(), m.processGeneration)

	case pingResultsMsg:
		m.accountsPinging = false
		m.accountsPings = msg.results
		m.wizardProviderOverrideVisible = m.wizardMode && m.wizardHasProvider() && !m.wizardHasOperationalProvider()
		return m, nil

	case oauthURLMsg:
		if msg.err != nil {
			m.accountsOAuthPending = false
			m.accountsStatus = "erro ao iniciar oauth: " + msg.err.Error()
			return m, nil
		}
		m.accountsOAuthURL = msg.url
		openBrowser(msg.url)
		ctx, cancel := context.WithCancel(context.Background())
		m.accountsOAuthCancel = cancel
		if msg.waitPermanent != nil {
			return m, waitOAuthPermanentCmd(ctx, msg.acctID, msg.waitPermanent)
		}
		return m, waitOAuthRefreshableCmd(ctx, msg.acctID, msg.waitRefreshable)

	case oauthResultMsg:
		m.accountsOAuthPending = false
		if m.accountsOAuthCancel != nil {
			m.accountsOAuthCancel()
			m.accountsOAuthCancel = nil
		}
		if msg.err != nil {
			m.accountsStatus = "oauth falhou: " + msg.err.Error()
			return m, nil
		}
		if m.credStore != nil {
			status, err := saveOAuthResult(m.credStore, msg)
			if err != nil {
				m.accountsStatus = "erro ao salvar: " + err.Error()
				return m, nil
			}
			m.accountsStatus = status
			if m.wizardMode {
				m.accountsPinging = true
				return m, pingAccountsCmd(m.credStore, m.customProviders)
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

	case toolSettingsUpdatedMsg:
		if msg.err != nil {
			m.toolsStatus = "erro ao aplicar no daemon: " + msg.err.Error()
			m.wizardToolSettingsPending = false
			return m, nil
		}
		m.toolsStatus = "configuração aplicada ao daemon atual."
		if m.wizardToolSettingsPending {
			m.wizardToolSettingsPending = false
			m.phase = phaseWizardSystemCheck
			m.wizardStep = 7
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

	case clipboardSequenceClearMsg:
		m.clipboardSequence = ""
		return m, nil

	case copyNoticeClearMsg:
		if msg.revision == m.copyNoticeRevision {
			m.copyNotice = ""
		}
		return m, nil

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
		if m.accountsOAuthPending && m.accountsOAuthCancel != nil {
			m.accountsOAuthCancel()
		}
		if m.wizardMode {
			m.wizardCancelled = true
		}
		return m, tea.Quit
	}
	if m.phase == phaseSplash {
		switch msg.String() {
		case "enter", " ", "esc":
			return m.finishSplash()
		default:
			return m, nil
		}
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
	if m.phase == phaseWizardEnvironment {
		return m.handleWizardEnvironmentKey(msg)
	}
	if m.phase == phaseWizardProjects {
		return m.handleWizardProjectsKey(msg)
	}
	if m.phase == phaseWizardRouting {
		return m.handleWizardRoutingKey(msg)
	}
	if m.phase == phaseWizardPermissions {
		return m.handleWizardPermissionsKey(msg)
	}
	if m.phase == phaseWizardToolsPreset {
		return m.handleWizardToolsPresetKey(msg)
	}
	if m.phase == phaseWizardSystemCheck {
		return m.handleWizardSystemCheckKey(msg)
	}
	if m.phase == phaseWizardSummary {
		return m.handleWizardSummaryKey(msg)
	}
	if m.question != nil {
		return m.handleQuestionKey(msg)
	}
	if m.approval != nil {
		return m.handleApprovalKey(msg)
	}

	switch msg.String() {
	case "ctrl+s":
		return m.togglePanel(panelStrategyPicker)

	case "ctrl+p":
		return m.togglePanel(panelStrategy)

	case "ctrl+t":
		return m.togglePanel(panelContext)

	case "ctrl+r":
		return m.togglePanel(panelRoute)

	case "ctrl+b":
		return m.togglePanel(panelProcesses)

	case "esc":
		if m.active == panelProcesses {
			m.closeProcessPanel()
			return m, nil
		}
		if m.active != panelNone {
			m.active = panelNone
			m.syncViewportSize()
			m.syncTranscriptRenderer()
		}
		return m, nil

	case "tab":
		if m.active == panelProcesses {
			return m.selectAdjacentProcess(1)
		}

	case "shift+tab":
		if m.active == panelProcesses {
			return m.selectAdjacentProcess(-1)
		}

	case "pgup":
		if m.active == panelProcesses {
			m.processFollow = false
			m.processViewport.HalfViewUp()
			return m, nil
		}

	case "pgdown":
		if m.active == panelProcesses {
			m.processViewport.HalfViewDown()
			m.resumeProcessFollowIfAtBottom()
			return m, nil
		}

	case "home":
		if m.active == panelProcesses {
			m.processFollow = false
			m.processViewport.GotoTop()
			return m, nil
		}

	case "end":
		if m.active == panelProcesses {
			m.processFollow = true
			m.processNewBytes = 0
			m.processViewport.GotoBottom()
			return m, nil
		}

	case "up":
		if m.active == panelProcesses {
			m.processFollow = false
			m.processViewport.LineUp(1)
			return m, nil
		}
		if m.active == panelStrategy {
			if m.strategyFocus > 0 {
				m.strategyFocus--
			}
			return m, nil
		}
		if m.active == panelStrategyPicker {
			if m.strategyPickerFocus > 0 {
				m.strategyPickerFocus--
			}
			return m, nil
		}
		m.viewport.LineUp(1)
		return m, nil

	case "down":
		if m.active == panelProcesses {
			m.processViewport.LineDown(1)
			m.resumeProcessFollowIfAtBottom()
			return m, nil
		}
		if m.active == panelStrategy {
			combo := m.currentCombo()
			if combo != nil && m.strategyFocus < len(combo.Providers)-1 {
				m.strategyFocus++
			}
			return m, nil
		}
		if m.active == panelStrategyPicker {
			if n := len(m.availableStrategies()); m.strategyPickerFocus < n-1 {
				m.strategyPickerFocus++
			}
			return m, nil
		}
		m.viewport.LineDown(1)
		return m, nil

	case "enter":
		if m.active == panelProcesses {
			return m, nil
		}
		if m.active == panelStrategyPicker {
			return m.applyFocusedStrategy()
		}
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
		m.accountsPinging = true
		return m, pingAccountsCmd(m.credStore, m.customProviders)
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

// handleMouse owns wheel scrolling, drag-to-copy inside the transcript, and
// the footer's clickable context badge. Terminal-native selection is not
// available while a TUI has mouse tracking enabled, so drag selection is
// deliberately implemented here instead of requiring the terminal-specific
// Shift bypass.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.phase == phaseSplash {
		return m, nil
	}
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y == 0 && m.routeBarStrategyLabel() != "" {
		if msg.X >= 0 && msg.X < m.routeBarStrategyWidth() {
			return m.togglePanel(panelStrategyPicker)
		}
	}
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && m.active == panelStrategyPicker {
		if index, ok := m.strategyPickerIndexAtRow(msg.Y); ok {
			m.strategyPickerFocus = index
			return m.applyFocusedStrategy()
		}
	}
	viewportRow := msg.Y - routeBarHeight
	insideBody := viewportRow >= 0 && viewportRow < m.viewport.Height
	inProcessPane := m.active == panelProcesses && insideBody && (!m.processUsesTile() || msg.X > m.viewport.Width)
	processX := msg.X
	if m.processUsesTile() {
		processX -= m.viewport.Width + 1
	}
	processOutputRow := viewportRow - m.processOutputStartRow()

	if msg.Button == tea.MouseButtonWheelUp {
		if inProcessPane {
			m.processFollow = false
			m.processViewport.LineUp(3)
			return m, nil
		}
		m.viewport.LineUp(3)
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelDown {
		if inProcessPane {
			m.processViewport.LineDown(3)
			m.resumeProcessFollowIfAtBottom()
			return m, nil
		}
		m.viewport.LineDown(3)
		return m, nil
	}

	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button == tea.MouseButtonLeft && inProcessPane {
			indices := m.visibleProcessIndices()
			listIndex := viewportRow - 1
			if listIndex >= 0 && listIndex < len(indices) {
				return m.selectProcess(m.processes[indices[listIndex]].ID)
			}
			if processOutputRow >= 0 && processOutputRow < m.processViewport.Height {
				m.copyNoticeRevision++
				m.copyNotice = ""
				m.selectionInProcess = true
				m.selection = beginTextSelection(m.processViewport.View(), processX, processOutputRow)
			}
			return m, nil
		}
		insideTranscript := insideBody && m.active != panelProcesses || (insideBody && m.processUsesTile() && msg.X < m.viewport.Width)
		if msg.Button == tea.MouseButtonLeft && insideTranscript {
			m.copyNoticeRevision++ // invalidate a previous copy's pending clear timer
			m.copyNotice = ""
			m.selectionInProcess = false
			m.selection = beginTextSelection(m.viewport.View(), msg.X, viewportRow)
			return m, nil
		}
	case tea.MouseActionMotion:
		if m.selection.active {
			if m.selectionInProcess {
				m.selection.move(processX, clampInt(processOutputRow, 0, m.processViewport.Height-1))
			} else {
				m.selection.move(msg.X, clampInt(viewportRow, 0, m.viewport.Height-1))
			}
			m.copyNotice = fmt.Sprintf("selecionando %d caracteres…", len([]rune(m.selection.text())))
			return m, nil
		}
	case tea.MouseActionRelease:
		if m.selection.active {
			if m.selectionInProcess {
				m.selection.move(processX, clampInt(processOutputRow, 0, m.processViewport.Height-1))
			} else {
				m.selection.move(msg.X, clampInt(viewportRow, 0, m.viewport.Height-1))
			}
			selected := m.selection.text()
			m.selection.active = false
			if selected == "" {
				m.copyNotice = ""
				if !m.selectionInProcess {
					row := m.viewport.YOffset + clampInt(viewportRow, 0, m.viewport.Height-1)
					if id := m.processLinkRows[row]; id != "" {
						return m.openProcessPanel(id)
					}
				}
				return m, nil
			}
			m.clipboardSequence = ansi.SetSystemClipboard(selected)
			m.copyNoticeRevision++
			m.copyNotice = fmt.Sprintf("✓ copiado · %d caracteres", len([]rune(selected)))
			return m, tea.Batch(clearClipboardSequenceCmd(), clearCopyNoticeCmd(m.copyNoticeRevision))
		}
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	footerRow := m.height - 1
	if msg.Y != footerRow {
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
		if p == panelProcesses {
			m.closeProcessPanel()
		} else {
			m.active = panelNone
			m.syncViewportSize()
			m.syncTranscriptRenderer()
		}
		return m, nil
	}
	if p == panelProcesses {
		return m.openProcessPanel(m.processSelected)
	}
	if m.active == panelProcesses {
		m.processGeneration++
	}
	m.active = p
	m.syncViewportSize()
	m.syncTranscriptRenderer()
	switch p {
	case panelStrategy:
		m.strategyFocus = 0
		return m, fetchStatusCmd(m.gateway)
	case panelStrategyPicker:
		m.strategyPickerErr = nil
		m.syncStrategyPickerFocus()
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
	m.workState = workPreparing
	m.activeTool = ""
	m.heartbeats = 0
	m.segment = 1
	m.segments = 0
	m.err = nil
	m.routeRunning = false
	m.routeCall = nil
	m.refreshTranscript()
	m.viewport.GotoBottom()
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
		m.workState = workWriting
		if lm := last(); lm != nil {
			lm.Content += msg.event.Content
		}
		m.refreshTranscript()

	case "tool_start":
		m.workState = workToolActive
		m.activeTool = msg.event.Name
		m.toolStartedAt = time.Now()
		if lm := last(); lm != nil {
			lm.ToolActivity = append(lm.ToolActivity, daemonclient.ToolActivity{
				Name: msg.event.Name, Args: msg.event.Args, Running: true,
			})
		}
		m.refreshTranscript()

	case "tool_result":
		m.workState = workAnalyzingResult
		m.activeTool = ""
		if lm := last(); lm != nil {
			for i := len(lm.ToolActivity) - 1; i >= 0; i-- {
				if lm.ToolActivity[i].Name == msg.event.Name && lm.ToolActivity[i].Running {
					lm.ToolActivity[i].Result = msg.event.Result
					lm.ToolActivity[i].OK = msg.event.OK
					lm.ToolActivity[i].ProcessID = msg.event.ProcessID
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

	case "approval":
		// Same "still blocked server-side" shape as question above.
		m.approval = &pendingApproval{id: msg.event.ApprovalID, tool: msg.event.Tool, subject: msg.event.Subject, options: msg.event.Options}
		m.refreshTranscript()

	case "route_start":
		// A model call is going out — real per-attempt progress isn't
		// observable until it finishes (the gateway's own fallback loop
		// happens inside one HTTP round-trip), so the route bar animates
		// the known candidate count until route_done lands.
		m.routeRunning = true
		m.workState = workModelActive

	case "heartbeat":
		// A payload-free heartbeat is still a real fact: the buffered model
		// call and daemon stream are alive. It advances the pulse counter but
		// deliberately invents no finer-grained provider/model activity.
		m.heartbeats++

	case "segment":
		if msg.event.Segment > 0 && msg.event.Segments >= msg.event.Segment {
			m.segment = msg.event.Segment
			m.segments = msg.event.Segments
		}

	case "route_done":
		m.routeRunning = false
		m.activeTool = ""
		m.routeCall = msg.event.RouteCall

	case "done":
		m.waiting = false
		m.err = nil
		m.routeRunning = false
		m.routeTrace = msg.event.RouteTrace
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
		m.routeRunning = false
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
	reserved := routeBarHeight + footerHeight + inputHeight
	if m.active != panelNone && m.active != panelProcesses {
		reserved += m.panelHeight()
	}
	h := m.height - reserved
	if h < 3 {
		h = 3
	}
	m.viewport.Width = m.chatViewportWidth()
	m.viewport.Height = h
	m.syncProcessViewport()
	m.input.SetWidth(m.width - 2)
}

func (m *Model) syncTranscriptRenderer() {
	width := m.viewport.Width
	if width < 1 {
		width = 1
	}
	if m.mdRenderer == nil || m.mdWidth != width {
		m.mdRenderer = newMarkdownRenderer(width)
		m.mdWidth = width
		m.refreshTranscript()
	}
}

func (m *Model) panelHeight() int {
	// 1/3 with a floor of 9 (not 1/4 with a floor of 6) — the strategy
	// panel's score-breakdown view (six factor lines plus header, other-
	// candidate scores, and a hint line) needs more room than the plain
	// provider-list view did; the route panel can also run several lines
	// per model call. A too-short panel truncates content rather than
	// wrapping or corrupting layout, so this is a real (if imperfect)
	// tradeoff against leaving less room for the transcript.
	h := m.height / 3
	if h < 9 {
		h = 9
	}
	return h
}
