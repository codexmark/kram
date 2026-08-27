// First-run setup wizard — an 8-step onboarding flow that runs the first
// time Kram starts with no completed setup (see internal/onboarding and
// cmd/kram's -setup flag). Technically split into two stages because
// gateway/daemon can only start once a valid config exists, and the
// tools/skills step needs a live daemon connection to list what's really
// registered — but every step shares one numbered header so it reads as
// one continuous flow to the user (see DECISIONS.md, "First-run wizard").
//
// Stage 1 (steps 1-5: Environment, Projects, Providers, Routing,
// Permissions) runs as a second, standalone tea.Program (RunWizard),
// entirely before the daemon/gateway exist — nothing in it may touch
// m.daemon/m.gateway. The Providers step reuses phaseAccounts itself
// (see accounts.go's wizardMode branches) rather than a third phase.
//
// Stage 2 (steps 6-8: Tools & Skills, System Check, Summary) runs inside
// the normal, post-daemon program, entered directly via app.New's
// openOnToolsPreset instead of the picker.
package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/kramhome"
	"github.com/codexmark/kram/internal/mcp"
	"github.com/codexmark/kram/internal/onboarding"
	"github.com/codexmark/kram/internal/providercatalog"
	"github.com/codexmark/kram/internal/toolsettings"
)

// ---- Step 1: Welcome (environment overview) ----

func (m Model) renderWizardEnvironment() string {
	var b strings.Builder
	// The greeting leads, in a readable color — it's the most human moment
	// of onboarding and used to be a faint afterthought under an env dump.
	b.WriteString(styleBody.Render(wizardEnvWelcome) + "\n\n")
	row := func(label, value string) {
		b.WriteString(styleMeta.Render(fmt.Sprintf("%-12s", label)) + styleBody.Render(value) + "\n")
	}
	row(wizardEnvSystemLabel, runtime.GOOS)
	row(wizardEnvCurrentDirLabel, m.wizardCWD)
	git := styleBadgeIdle.Render("● "+wizardEnvGitNotFound) + "  " + styleWizardDim.Render(wizardEnvGitOptionalNote)
	if m.wizardHasGit {
		git = styleBadgeOK.Render("● " + wizardEnvGitFound)
	}
	b.WriteString(styleMeta.Render(fmt.Sprintf("%-12s", "Git")) + git + "\n")
	row("Home", m.wizardHomeDir)
	return m.renderWizardFrame(1, wizardTitleWelcome, b.String(),
		[]wizardKey{{"enter", "start"}, {"esc", "quit setup"}}, 0)
}

func (m Model) handleWizardEnvironmentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.wizardCancelled = true
		return m, tea.Quit
	case "enter":
		if m.wizardWorkspaceLocked {
			m.phase = phaseAccounts
			m.wizardStep = 3
			m.accountsPinging = true
			return m, pingAccountsCmd(m.credStore, m.customProviders)
		}
		m.phase = phaseWizardProjects
		m.wizardStep = 2
		return m, textinput.Blink
	}
	return m, nil
}

// ---- Step 2: Projects (Projects Root + Workspace) ----

func (m Model) renderWizardProjects() string {
	var b strings.Builder
	field := func(label, hint, view string, focused bool) {
		rendered := styleMeta.Render(label)
		if focused {
			rendered = styleBadgeAccent.Bold(true).Render(label)
		}
		b.WriteString(rendered + "\n")
		b.WriteString(styleWizardDim.Render(hint) + "\n")
		b.WriteString(view + "\n\n")
	}
	field("Projects root", wizardProjectsRootHint, m.wizardProjectsRootInput.View(), m.wizardProjectsField == 0)
	field("Workspace", wizardProjectsWorkspaceHint, m.wizardWorkspaceInput.View(), m.wizardProjectsField == 1)
	if m.wizardWorkspaceErr != nil {
		b.WriteString(styleErrBadge.Render(wizardErrPrefix+m.wizardWorkspaceErr.Error()) + "\n")
	}
	return m.renderWizardFrame(2, wizardTitleProjects, b.String(),
		[]wizardKey{{"tab", "switch field"}, {"enter", "continue"}, {"esc", "back"}}, 0)
}

func (m Model) handleWizardProjectsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.phase = phaseWizardEnvironment
		m.wizardStep = 1
		return m, nil
	case "tab":
		m.wizardProjectsField = 1 - m.wizardProjectsField
		if m.wizardProjectsField == 0 {
			m.wizardProjectsRootInput.Focus()
			m.wizardWorkspaceInput.Blur()
		} else {
			m.wizardWorkspaceInput.Focus()
			m.wizardProjectsRootInput.Blur()
		}
		return m, nil
	case "enter":
		root := expandTilde(strings.TrimSpace(m.wizardProjectsRootInput.Value()))
		ws := expandTilde(strings.TrimSpace(m.wizardWorkspaceInput.Value()))
		if ws == "" {
			ws = root
		}
		absWS, err := filepath.Abs(ws)
		if err != nil {
			m.wizardWorkspaceErr = err
			return m, nil
		}
		if err := os.MkdirAll(filepath.Join(absWS, ".kram"), 0o755); err != nil {
			m.wizardWorkspaceErr = fmt.Errorf(wizardErrCreateWorkspaceFmt, absWS, err)
			return m, nil
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			absRoot = root // best-effort — this is only a suggestion persisted for later, never validated
		}
		m.wizardWorkspaceErr = nil
		m.wizardChosenWorkspace = absWS
		m.wizardProjectsRoot = absRoot
		m.phase = phaseAccounts
		m.wizardStep = 3
		m.accountsPinging = true
		return m, pingAccountsCmd(m.credStore, m.customProviders)
	}
	var cmd tea.Cmd
	if m.wizardProjectsField == 0 {
		m.wizardProjectsRootInput, cmd = m.wizardProjectsRootInput.Update(msg)
	} else {
		m.wizardWorkspaceInput, cmd = m.wizardWorkspaceInput.Update(msg)
	}
	return m, cmd
}

// ---- Step 4: Routing ----

type wizardRoutingOption struct {
	label, strategy, desc string
}

var wizardRoutingOptions = []wizardRoutingOption{
	{label: wizardRoutingAutoLabel, strategy: "", desc: wizardRoutingAutoDesc},
	{label: "Smart", strategy: "smart", desc: wizardRoutingSmartDesc},
	{label: "Round Robin", strategy: "round-robin", desc: wizardRoutingRoundRobinDesc},
}

func (m Model) renderWizardRouting() string {
	labels := make([]string, len(wizardRoutingOptions))
	descs := make([]string, len(wizardRoutingOptions))
	for i, opt := range wizardRoutingOptions {
		labels[i], descs[i] = opt.label, opt.desc
	}
	var b strings.Builder
	b.WriteString(renderWizardOptions(labels, descs, m.wizardRoutingCursor) + "\n")
	if wizardRoutingOptions[m.wizardRoutingCursor].strategy == "" {
		resolved := "SMART"
		if m.wizardConfiguredProviderCount() <= 1 {
			resolved = "SINGLE PROVIDER"
		}
		b.WriteString(styleMeta.Render(wizardRoutingAutoPreviewPrefix) + styleBadgeOK.Render(resolved) + "\n\n")
	}
	b.WriteString(styleWizardDim.Render(wizardRoutingHint))
	return m.renderWizardFrame(4, wizardTitleRouting, b.String(), wizardKeysChoose, 0)
}

func (m Model) handleWizardRoutingKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.phase = phaseAccounts
		m.wizardStep = 3
	case "up":
		if m.wizardRoutingCursor > 0 {
			m.wizardRoutingCursor--
		}
	case "down":
		if m.wizardRoutingCursor < len(wizardRoutingOptions)-1 {
			m.wizardRoutingCursor++
		}
	case "enter":
		m.wizardChosenStrategy = wizardRoutingOptions[m.wizardRoutingCursor].strategy
		m.phase = phaseWizardPermissions
		m.wizardStep = 5
	}
	return m, nil
}

// wizardConfiguredProviderCount counts how many gateway providers are
// configured right now — every catalog entry backed by a real credential
// (mirroring gatewayconfig.catalogProviderConfig, so one shared key like
// OpenRouter's counts once per free model it fans out into) plus every
// registered custom provider — so the "Auto currently resolves to" preview
// matches what gatewayconfig.autoStrategy will pick from the same count (one
// provider → single/no routing; two or more → smart).
func (m Model) wizardConfiguredProviderCount() int {
	n := 0
	for _, p := range providercatalog.Providers {
		if os.Getenv(p.EnvVar) != "" {
			n++
			continue
		}
		if m.credStore != nil {
			if m.credStore.Get(p.EnvVar) != "" {
				n++
				continue
			}
			if _, ok := m.credStore.GetOAuth(p.EnvVar); ok {
				n++
			}
		}
	}
	return n + len(m.customProviders)
}

// ---- Step 5: Permissions ----

type wizardPermOption struct {
	label, key, desc string
}

var wizardPermOptions = []wizardPermOption{
	{label: "Recommended", key: "recommended", desc: wizardPermRecommendedDesc},
	{label: "Strict", key: "strict", desc: wizardPermStrictDesc},
	{label: "Autonomous", key: "autonomous", desc: wizardPermAutonomousDesc},
}

func (m Model) renderWizardPermissions() string {
	labels := make([]string, len(wizardPermOptions))
	descs := make([]string, len(wizardPermOptions))
	for i, opt := range wizardPermOptions {
		labels[i], descs[i] = opt.label, opt.desc
	}
	body := renderWizardOptions(labels, descs, m.wizardPermCursor)
	return m.renderWizardFrame(5, wizardTitlePermissions, body, wizardKeysChoose, 0)
}

func (m Model) handleWizardPermissionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.phase = phaseWizardRouting
		m.wizardStep = 4
	case "up":
		if m.wizardPermCursor > 0 {
			m.wizardPermCursor--
		}
	case "down":
		if m.wizardPermCursor < len(wizardPermOptions)-1 {
			m.wizardPermCursor++
		}
	case "enter":
		m.wizardChosenPermPreset = wizardPermOptions[m.wizardPermCursor].key
		m.wizardDone = true
		return m, tea.Quit
	}
	return m, nil
}

// ---- Stage 1 orchestration ----

// WizardResult is what Stage 1 hands back to cmd/kram's main() once its
// standalone program exits.
type WizardResult struct {
	Cancelled       bool
	BootSplashShown bool // prevents Stage 2 from replaying boot in the same invocation
	Workspace       string
	ProjectsRoot    string
	Strategy        string // "" (Auto), "smart", "round-robin"
	PermPreset      string // "recommended", "strict", "autonomous"
}

// RunWizard runs Stage 1 (steps 1-5) as its own standalone Bubble Tea
// program — no daemon/gateway exist yet at this point, which is exactly
// why this can't just be a phase inside the normal post-daemon program.
// explicitWorkspace/workspaceExplicit mirror whatever -workspace the user
// passed (or didn't) on the command line.
func RunWizard(explicitWorkspace string, workspaceExplicit bool) (WizardResult, error) {
	m := newWizardModel(explicitWorkspace, workspaceExplicit)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return WizardResult{}, err
	}
	fm, ok := final.(Model)
	if !ok || fm.wizardCancelled || !fm.wizardDone {
		return WizardResult{Cancelled: true, BootSplashShown: true}, nil
	}
	return WizardResult{
		BootSplashShown: true,
		Workspace:       fm.wizardChosenWorkspace,
		ProjectsRoot:    fm.wizardProjectsRoot,
		Strategy:        fm.wizardChosenStrategy,
		PermPreset:      fm.wizardChosenPermPreset,
	}, nil
}

func newWizardModel(explicitWorkspace string, workspaceExplicit bool) Model {
	credStore, _ := credentials.Load() // nil on failure; accounts.go already guards every use
	customStore, _ := customprovider.Load()
	var customProviders []customprovider.Provider
	if customStore != nil {
		customProviders = customStore.All()
	}

	cwd, _ := os.Getwd()
	hasGit, cwdIsRepo := wizardEnvironment(cwd)
	home, _ := os.UserHomeDir()

	rootInput := textinput.New()
	rootInput.Prompt = "› "
	rootInput.CharLimit = 300
	rootInput.SetValue(suggestedProjectsRoot(home))
	rootInput.Focus()

	wsDefault := suggestedProjectsRoot(home)
	if cwdIsRepo && cwd != "" {
		wsDefault = cwd
	}
	wsInput := textinput.New()
	wsInput.Prompt = "› "
	wsInput.CharLimit = 300
	wsInput.SetValue(wsDefault)

	m := Model{
		wizardMode:              true,
		wizardWorkspaceLocked:   workspaceExplicit,
		wizardStep:              1,
		wizardCWD:               cwd,
		wizardHasGit:            hasGit,
		wizardHomeDir:           home,
		wizardProjectsRootInput: rootInput,
		wizardWorkspaceInput:    wsInput,
		phase:                   phaseSplash,
		splashTarget:            phaseWizardEnvironment,
		credStore:               credStore,
		accountsKeyInput:        newAccountsKeyInput(),
		customStore:             customStore,
		customProviders:         customProviders,
	}
	if workspaceExplicit {
		if abs, err := filepath.Abs(explicitWorkspace); err == nil {
			m.wizardChosenWorkspace = abs
		}
	}
	return m
}

// wizardEnvironment keeps two different facts separate: whether Git is
// installed (shown on the Environment screen) and whether the current
// directory is already a repository (used only as the workspace default).
// Conflating them made a fresh Termux home report "Git not found" even after
// the git package had been installed successfully.
func wizardEnvironment(cwd string) (hasGit, cwdIsRepo bool) {
	_, gitErr := exec.LookPath("git")
	hasGit = gitErr == nil
	if cwd != "" {
		_, statErr := os.Stat(filepath.Join(cwd, ".git"))
		cwdIsRepo = statErr == nil
	}
	return hasGit, cwdIsRepo
}

// suggestedProjectsRoot follows the same convention on Linux and macOS
// (Kram has no per-platform code path beyond this switch — it's purely a
// suggested default the user can freely overwrite) and a Windows-idiomatic
// one there. Never created eagerly — only os.MkdirAll'd once the user
// actually confirms a workspace under it.
func suggestedProjectsRoot(home string) string {
	if home == "" {
		home = "."
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "Documents", "Projects")
	}
	return filepath.Join(home, "Projects")
}

func expandTilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return filepath.Join(home, p[2:])
	}
	return p
}

// ---- Step 6: Tools & Skills preset (Stage 2 — needs a live daemon) ----

type wizardToolsPresetOption struct {
	label, key, desc string
}

var wizardToolsPresetOptions = []wizardToolsPresetOption{
	{label: "Recommended", key: "recommended", desc: wizardToolsRecommendedDesc},
	{label: "Minimal", key: "minimal", desc: wizardToolsMinimalDesc},
	{label: "Custom", key: "custom", desc: wizardToolsCustomDesc},
}

// wizardMinimalSafeTools is what the "Minimal" preset keeps on — read,
// search, navigation, and code-intelligence only, nothing that writes,
// executes, or reaches outside the workspace. Anything the daemon
// reports that isn't in this list (including every MCP tool, since a
// remote server's capabilities aren't something Kram can vouch for as
// "safe") gets disabled.
var wizardMinimalSafeTools = map[string]bool{
	"read_file": true, "list_dir": true, "glob": true, "grep": true,
	"git_status": true, "git_diff": true,
	"lsp_diagnostics": true, "lsp_definition": true, "lsp_references": true,
	"memory_search": true, "session_search": true,
	"skill_list": true, "skill": true,
	"artifact_read": true, "web_fetch": true,
}

// wizardSkillsOptions are the starter-pack offer's two choices.
var wizardSkillsOptions = []wizardToolsPresetOption{
	{label: "Install starter pack", key: "install", desc: wizardSkillsInstallDesc},
	{label: "Skip", key: "skip", desc: wizardSkillsSkipDesc},
}

// renderWizardSkillsOffer is step 6's second half (#135): with the tool
// preset applied, offer the curated starter skills before System Check.
func (m Model) renderWizardSkillsOffer() string {
	var b strings.Builder
	b.WriteString(styleMeta.Render(wizardSkillsIntro) + "\n\n")
	labels := make([]string, len(wizardSkillsOptions))
	descs := make([]string, len(wizardSkillsOptions))
	for i, opt := range wizardSkillsOptions {
		labels[i], descs[i] = opt.label, opt.desc
	}
	b.WriteString(renderWizardOptions(labels, descs, m.wizardSkillsCursor))
	if m.wizardSkillsInstalling {
		b.WriteString("\n" + styleBadgeWarn.Render(wizardSkillsInstalling))
	} else if m.toolsStatus != "" {
		b.WriteString("\n" + styleErrBadge.Render(m.toolsStatus))
	}
	return m.renderWizardFrame(6, wizardTitleTools, b.String(), wizardKeysChoose, 0)
}

func (m Model) handleWizardSkillsOfferKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.wizardSkillsInstalling {
		return m, nil
	}
	switch msg.String() {
	case "up":
		if m.wizardSkillsCursor > 0 {
			m.wizardSkillsCursor--
		}
	case "down":
		if m.wizardSkillsCursor < len(wizardSkillsOptions)-1 {
			m.wizardSkillsCursor++
		}
	case "esc":
		// Skip is always safe — skills install later via skill_install.
		m.wizardSkillsOffer = false
		m.phase = phaseWizardSystemCheck
		m.wizardStep = 7
	case "enter":
		if wizardSkillsOptions[m.wizardSkillsCursor].key == "skip" {
			m.wizardSkillsOffer = false
			m.phase = phaseWizardSystemCheck
			m.wizardStep = 7
			return m, nil
		}
		m.wizardSkillsInstalling = true
		m.toolsStatus = ""
		return m, installSkillPackCmd()
	}
	return m, nil
}

func (m Model) renderWizardToolsPreset() string {
	if m.wizardSkillsOffer {
		return m.renderWizardSkillsOffer()
	}
	if m.toolsLoading {
		return m.renderWizardFrame(6, wizardTitleTools,
			styleMeta.Render(m.spin.View()+wizardToolsLoading), nil, 0)
	}
	labels := make([]string, len(wizardToolsPresetOptions))
	descs := make([]string, len(wizardToolsPresetOptions))
	for i, opt := range wizardToolsPresetOptions {
		labels[i], descs[i] = opt.label, opt.desc
	}
	var b strings.Builder
	b.WriteString(styleMeta.Render(fmt.Sprintf(wizardToolsRegisteredFmt, len(m.toolToggleItems()))) + "\n\n")
	b.WriteString(renderWizardOptions(labels, descs, m.wizardToolsPresetCursor))
	if m.toolsStatus != "" {
		b.WriteString("\n" + styleMeta.Render(m.toolsStatus) + "\n")
	}
	return m.renderWizardFrame(6, wizardTitleTools, b.String(),
		[]wizardKey{{"↑↓", "choose"}, {"enter", "apply"}}, 0)
}

func (m Model) handleWizardToolsPresetKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.wizardSkillsOffer {
		return m.handleWizardSkillsOfferKey(msg)
	}
	if m.toolsLoading {
		return m, nil
	}
	switch msg.String() {
	case "up":
		if m.wizardToolsPresetCursor > 0 {
			m.wizardToolsPresetCursor--
		}
	case "down":
		if m.wizardToolsPresetCursor < len(wizardToolsPresetOptions)-1 {
			m.wizardToolsPresetCursor++
		}
	case "enter":
		opt := wizardToolsPresetOptions[m.wizardToolsPresetCursor]
		m.wizardChosenToolsPreset = opt.key
		switch opt.key {
		case "recommended", "minimal":
			if err := applyWizardToolPreset(m.toolSettings, m.toolToggleItems(), opt.key); err != nil {
				m.toolsStatus = wizardToolsErrSavePreset + err.Error()
				return m, nil
			}
			m.toolsStatus = wizardToolsApplying
			m.wizardToolSettingsPending = true
			return m, syncToolSettingsCmd(m.daemon, m.toolSettings)
		case "custom":
			m.phase = phaseTools
			m.toolsStatus = ""
			return m, nil
		}
	}
	return m, nil
}

func applyWizardToolPreset(settings *toolsettings.Store, items []toolToggleItem, preset string) error {
	if settings == nil {
		return fmt.Errorf(wizardErrToolsStorage)
	}
	var disabled []string
	if preset == "minimal" {
		for _, item := range items {
			if !wizardMinimalSafeTools[item.name] {
				disabled = append(disabled, item.name)
			}
		}
	}
	return settings.ReplaceDisabled(disabled)
}

// ---- Step 7: System Check (Stage 2) ----

type wizardSystemCheckResult struct {
	gitFound, goFound, goplsFound bool
	mcpServers                    int
	providersConfigured           int
}

// wizardComputeSystemCheck is cheap (a handful of exec.LookPath calls
// plus reading two already-loaded local stores) and has no state worth
// caching across renders, unlike the tools/skills list above which comes
// from a real daemon round-trip.
func (m Model) wizardComputeSystemCheck() wizardSystemCheckResult {
	var r wizardSystemCheckResult
	_, r.goFound = lookPath("go")
	_, r.gitFound = lookPath("git")
	_, r.goplsFound = lookPath("gopls")
	r.mcpServers = len(mcp.LoadConfig(m.workspace))
	for _, row := range m.accountRows() {
		if row.envSet || row.storedSet {
			r.providersConfigured++
		}
	}
	return r
}

func lookPath(name string) (string, bool) {
	p, err := exec.LookPath(name)
	return p, err == nil
}

func (m Model) renderWizardSystemCheck() string {
	r := m.wizardComputeSystemCheck()
	var b strings.Builder
	line := func(label string, ok bool, extra string) string {
		dot := styleBadgeIdle.Render("●")
		if ok {
			dot = styleBadgeOK.Render("●")
		}
		s := dot + " " + styleBody.Render(fmt.Sprintf("%-20s", label))
		if extra != "" {
			s += "  " + styleMeta.Render(extra)
		}
		return s
	}
	b.WriteString(line("Git", r.gitFound, "") + "\n")
	b.WriteString(line("Go", r.goFound, "") + "\n")
	b.WriteString(line("gopls", r.goplsFound, wizardCheckOptional) + "\n")
	b.WriteString(line("Workspace writable", true, m.workspace) + "\n")
	b.WriteString(line("Providers", r.providersConfigured > 0, fmt.Sprintf(wizardCheckProvidersFmt, r.providersConfigured)) + "\n")
	b.WriteString(line("MCP", true, fmt.Sprintf(wizardCheckMCPFmt, r.mcpServers)) + "\n\n")
	b.WriteString(styleWizardDim.Render(wizardCheckHint))
	return m.renderWizardFrame(7, wizardTitleCheck, b.String(),
		[]wizardKey{{"enter", "continue"}, {"esc", "back"}}, 0)
}

func (m Model) handleWizardSystemCheckKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.phase = phaseWizardSummary
		m.wizardStep = 8
	case "esc":
		// Back is available on every step — Check included, which used to
		// be the one screen with no way to revisit the Tools preset.
		m.phase = phaseWizardToolsPreset
		m.wizardStep = 6
	}
	return m, nil
}

// ---- Step 8: Summary / Ready (Stage 2) ----

func (m Model) renderWizardSummary() string {
	var b strings.Builder
	b.WriteString(styleBadgeOK.Bold(true).Render(wizardSummaryHeadline) + "\n\n")

	row := func(label, value string) string {
		return styleMeta.Render(fmt.Sprintf("%-14s", label)) + styleBody.Render(value) + "\n"
	}
	strategy := m.wizardChosenStrategy
	if strategy == "" {
		strategy = "auto"
	}
	toolsPreset := m.wizardChosenToolsPreset
	if toolsPreset == "" {
		toolsPreset = "recommended"
	}
	cfgPath, _ := kramhome.Path("config.yaml")

	b.WriteString(row("Workspace", m.workspace))
	b.WriteString(row("Routing", strategy))
	b.WriteString(row("Permissions", m.wizardChosenPermPreset))
	b.WriteString(row("Tools/Skills", toolsPreset))
	b.WriteString(row("Config", cfgPath))
	if m.wizardCompletionErr != nil {
		b.WriteString("\n" + styleErrBadge.Render(wizardErrCompletionPrefix+m.wizardCompletionErr.Error()) + "\n")
	}
	b.WriteString("\n" + styleWizardDim.Render(wizardSummaryNote))
	return m.renderWizardFrame(8, wizardTitleReady, b.String(),
		[]wizardKey{{"enter", "open your first session"}, {"esc", "back"}}, 0)
}

func (m Model) handleWizardSummaryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.phase = phaseWizardSystemCheck
		m.wizardStep = 7
		return m, nil
	}
	if msg.String() == "enter" {
		if err := onboarding.Save(onboarding.State{ProjectsRoot: m.wizardProjectsRoot, LastWorkspace: m.workspace}); err != nil {
			m.wizardCompletionErr = err
			return m, nil
		}
		m.wizardCompletionErr = nil
		title := filepath.Base(m.workspace)
		if title == "" || title == "." || title == string(filepath.Separator) {
			title = "kram"
		}
		m.wizardWelcomeSession = true
		return m, createSessionCmd(m.daemon, title)
	}
	return m, nil
}

// renderWizardWelcomeBanner is shown once, client-side only, above the
// empty transcript of the session the wizard just created — never
// appended to m.messages, so it's never persisted and never mistaken for
// a real model reply (see refreshTranscript in view.go).
func (m Model) renderWizardWelcomeBanner() string {
	var b strings.Builder
	b.WriteString(styleKramTag.Render("kram") + "  " + styleBody.Render(
		wizardWelcomeBody) + "\n\n")
	b.WriteString(styleHint.Render(wizardWelcomeSuggestion))
	return b.String()
}
