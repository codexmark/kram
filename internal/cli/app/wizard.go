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
	"github.com/codexmark/kram/internal/kramhome"
	"github.com/codexmark/kram/internal/mcp"
	"github.com/codexmark/kram/internal/providercatalog"
)

func renderWizardHeader(step int, title string) string {
	return styleMeta.Render(fmt.Sprintf("KRAM SETUP · %d/8 %s", step, title))
}

// ---- Step 1: Environment ----

func (m Model) renderWizardEnvironment() string {
	var b strings.Builder
	b.WriteString(renderWizardHeader(1, "Environment") + "\n\n")
	b.WriteString(fmt.Sprintf("%-16s %s\n", "Sistema", styleBody.Render(runtime.GOOS)))
	b.WriteString(fmt.Sprintf("%-16s %s\n", "Diretório atual", styleBody.Render(m.wizardCWD)))
	git := styleBadgeIdle.Render("não encontrado")
	if m.wizardHasGit {
		git = styleBadgeOK.Render("encontrado")
	}
	b.WriteString(fmt.Sprintf("%-16s %s\n", "Git", git))
	b.WriteString(fmt.Sprintf("%-16s %s\n\n", "Home", styleBody.Render(m.wizardHomeDir)))
	b.WriteString(styleHint.Render("bem-vindo ao kram. vamos configurar o essencial — leva menos de um minuto.") + "\n\n")
	b.WriteString(styleHint.Render("enter continua · esc/ctrl+c cancela"))
	return b.String()
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
			return m, pingAccountsCmd(m.credStore)
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
	b.WriteString(renderWizardHeader(2, "Projects") + "\n\n")
	b.WriteString(styleMeta.Render("Projects Root") + styleHint.Render("  (onde você costuma manter seus projetos)") + "\n")
	b.WriteString(m.wizardProjectsRootInput.View() + "\n\n")
	b.WriteString(styleMeta.Render("Workspace") + styleHint.Render("  (o projeto desta sessão)") + "\n")
	b.WriteString(m.wizardWorkspaceInput.View() + "\n\n")
	if m.wizardWorkspaceErr != nil {
		b.WriteString(styleErrBadge.Render("erro: "+m.wizardWorkspaceErr.Error()) + "\n\n")
	}
	b.WriteString(styleHint.Render("tab troca de campo · enter continua · esc volta"))
	return b.String()
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
			m.wizardWorkspaceErr = fmt.Errorf("não consegui criar %s: %w", absWS, err)
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
		return m, pingAccountsCmd(m.credStore)
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
	{label: "Auto (recomendado)", strategy: "", desc: "kram escolhe com base nos providers configurados"},
	{label: "Smart", strategy: "smart", desc: "saúde + confiabilidade + latência + afinidade de cache"},
	{label: "Round Robin", strategy: "round-robin", desc: "distribui as chamadas entre os providers elegíveis"},
}

func (m Model) renderWizardRouting() string {
	var b strings.Builder
	b.WriteString(renderWizardHeader(4, "Routing") + "\n\n")
	for i, opt := range wizardRoutingOptions {
		line := fmt.Sprintf("%-22s %s", opt.label, styleHint.Render(opt.desc))
		if i == m.wizardRoutingCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n")
	if wizardRoutingOptions[m.wizardRoutingCursor].strategy == "" {
		resolved := "PRIORITY"
		if !m.wizardHavePaidProvider() {
			resolved = "ROUND ROBIN"
		}
		b.WriteString(styleHint.Render("Auto currently resolves to: "+resolved) + "\n\n")
	}
	b.WriteString(styleHint.Render("estratégias avançadas, pesos e gates continuam ajustáveis no config.yaml gerado.") + "\n\n")
	b.WriteString(styleHint.Render("↑↓ escolher · enter continua · esc volta"))
	return b.String()
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

// wizardHavePaidProvider mirrors cmd/kram's own autoStrategy heuristic
// (a paid provider present favors stable priority order; free-tier-only
// favors round-robin) so the "Auto currently resolves to" line is the
// real answer, not a guess — it just can't call that unexported function
// directly across the main/library boundary, so the tiny lookup is
// duplicated here against the same providercatalog data it reads from.
func (m Model) wizardHavePaidProvider() bool {
	configured := make(map[string]bool)
	for i, row := range m.accountRows() {
		if row.envSet || row.storedSet {
			configured[providercatalog.Accounts[i].EnvVar] = true
		}
	}
	for _, p := range providercatalog.Providers {
		if configured[p.EnvVar] && !p.FreeTier {
			return true
		}
	}
	return false
}

// ---- Step 5: Permissions ----

type wizardPermOption struct {
	label, key, desc string
}

var wizardPermOptions = []wizardPermOption{
	{label: "Recommended", key: "recommended", desc: "pergunta antes de rm -rf, git push, apagar/mover arquivo — resto liberado"},
	{label: "Strict", key: "strict", desc: "pergunta antes de quase tudo, inclusive tools MCP — só leitura e git status liberados"},
	{label: "Autonomous", key: "autonomous", desc: "poucas confirmações — só bloqueia rm -rf com caminho absoluto"},
}

func (m Model) renderWizardPermissions() string {
	var b strings.Builder
	b.WriteString(renderWizardHeader(5, "Permissions") + "\n\n")
	for i, opt := range wizardPermOptions {
		line := fmt.Sprintf("%-14s %s", opt.label, styleHint.Render(opt.desc))
		if i == m.wizardPermCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n" + styleHint.Render("↑↓ escolher · enter continua · esc volta"))
	return b.String()
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
	Cancelled    bool
	Workspace    string
	ProjectsRoot string
	Strategy     string // "" (Auto), "smart", "round-robin"
	PermPreset   string // "recommended", "strict", "autonomous"
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
		return WizardResult{Cancelled: true}, nil
	}
	return WizardResult{
		Workspace:    fm.wizardChosenWorkspace,
		ProjectsRoot: fm.wizardProjectsRoot,
		Strategy:     fm.wizardChosenStrategy,
		PermPreset:   fm.wizardChosenPermPreset,
	}, nil
}

func newWizardModel(explicitWorkspace string, workspaceExplicit bool) Model {
	credStore, _ := credentials.Load() // nil on failure; accounts.go already guards every use

	cwd, _ := os.Getwd()
	hasGit := false
	if cwd != "" {
		if _, err := os.Stat(filepath.Join(cwd, ".git")); err == nil {
			hasGit = true
		}
	}
	home, _ := os.UserHomeDir()

	rootInput := textinput.New()
	rootInput.Prompt = "› "
	rootInput.CharLimit = 300
	rootInput.SetValue(suggestedProjectsRoot(home))
	rootInput.Focus()

	wsDefault := suggestedProjectsRoot(home)
	if hasGit && cwd != "" {
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
		phase:                   phaseWizardEnvironment,
		credStore:               credStore,
		accountsKeyInput:        newAccountsKeyInput(),
	}
	if workspaceExplicit {
		if abs, err := filepath.Abs(explicitWorkspace); err == nil {
			m.wizardChosenWorkspace = abs
		}
	}
	return m
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
	{label: "Recommended", key: "recommended", desc: "conjunto padrão de desenvolvimento — nada desligado"},
	{label: "Minimal", key: "minimal", desc: "leitura, busca, navegação e inteligência de código — resto desligado"},
	{label: "Custom", key: "custom", desc: "escolher individualmente"},
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

func (m Model) renderWizardToolsPreset() string {
	var b strings.Builder
	b.WriteString(renderWizardHeader(6, "Tools & Skills") + "\n\n")
	if m.toolsLoading {
		b.WriteString(styleMeta.Render(m.spin.View()+" carregando…") + "\n\n")
		return b.String()
	}
	items := m.toolToggleItems()
	b.WriteString(styleHint.Render(fmt.Sprintf("%d tools/skills registrados.", len(items))) + "\n\n")
	for i, opt := range wizardToolsPresetOptions {
		line := fmt.Sprintf("%-14s %s", opt.label, styleHint.Render(opt.desc))
		if i == m.wizardToolsPresetCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n" + styleHint.Render("↑↓ escolher · enter aplica · Custom abre a tela individual"))
	return b.String()
}

func (m Model) handleWizardToolsPresetKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		case "minimal":
			if m.toolSettings != nil {
				var disable []string
				for _, it := range m.toolToggleItems() {
					if !wizardMinimalSafeTools[it.name] {
						disable = append(disable, it.name)
					}
				}
				_ = m.toolSettings.SetAllDisabled(disable, true)
			}
		case "custom":
			m.phase = phaseTools
			m.toolsStatus = ""
			return m, nil
		}
		m.phase = phaseWizardSystemCheck
		m.wizardStep = 7
	}
	return m, nil
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
	b.WriteString(renderWizardHeader(7, "System Check") + "\n\n")
	line := func(label string, ok bool, extra string) string {
		dot := styleBadgeIdle.Render("●")
		if ok {
			dot = styleBadgeOK.Render("●")
		}
		s := fmt.Sprintf("%s %-20s", dot, label)
		if extra != "" {
			s += "  " + styleHint.Render(extra)
		}
		return s
	}
	b.WriteString(line("Git", r.gitFound, "") + "\n")
	b.WriteString(line("Go", r.goFound, "") + "\n")
	b.WriteString(line("gopls", r.goplsFound, "opcional") + "\n")
	b.WriteString(line("Workspace writable", true, m.workspace) + "\n")
	b.WriteString(line("Providers", r.providersConfigured > 0, fmt.Sprintf("%d configurado(s)", r.providersConfigured)) + "\n")
	b.WriteString(line("MCP", true, fmt.Sprintf("%d servidor(es) configurado(s)", r.mcpServers)) + "\n\n")
	b.WriteString(styleHint.Render("itens opcionais não bloqueiam — só informativo.") + "\n\n")
	b.WriteString(styleHint.Render("enter continua"))
	return b.String()
}

func (m Model) handleWizardSystemCheckKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		m.phase = phaseWizardSummary
		m.wizardStep = 8
	}
	return m, nil
}

// ---- Step 8: Summary / Ready (Stage 2) ----

func (m Model) renderWizardSummary() string {
	var b strings.Builder
	b.WriteString(renderWizardHeader(8, "Ready") + "\n\n")
	b.WriteString(styleBadgeOK.Bold(true).Render("KRAM IS READY") + "\n\n")

	row := func(label, value string) string {
		return fmt.Sprintf("%-16s %s\n", label, styleBody.Render(value))
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
	b.WriteString("\n")
	b.WriteString(styleHint.Render("enter abre uma sessão e começa a usar o kram · kram --setup reconfigura a qualquer momento"))
	return b.String()
}

func (m Model) handleWizardSummaryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
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
		"Kram está pronto. Posso começar mapeando este projeto, revisar uma tarefa ou trabalhar diretamente em uma alteração.") + "\n\n")
	b.WriteString(styleHint.Render("sugestão: \"Map this repository and explain its architecture\""))
	return b.String()
}
