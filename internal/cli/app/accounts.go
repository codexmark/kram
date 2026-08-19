package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/oauthflow"
	"github.com/codexmark/kram/internal/providercatalog"
	"github.com/codexmark/kram/internal/providerping"
)

// accountStatus is what the accounts screen shows for one catalog entry.
// The shell environment always wins over a stored key (see cmd/kram's
// startup wiring, which only os.Setenv's a stored key when the real env
// var isn't already set) — so a row can show "set (ambiente)" even when
// nothing is stored here, and that distinction matters enough to the user
// to show explicitly rather than collapsing both into one "configured".
type accountStatus struct {
	envSet    bool
	storedSet bool
}

// newAccountsKeyInput builds the masked textinput.Model used for pasting
// an API key — shared by the normal accounts screen (New) and the
// wizard's own model constructor (newWizardModel), so the two never
// drift apart.
func newAccountsKeyInput() textinput.Model {
	keyInput := textinput.New()
	keyInput.Placeholder = "sk-…"
	keyInput.CharLimit = 400
	keyInput.Prompt = "› "
	keyInput.EchoMode = textinput.EchoPassword
	keyInput.EchoCharacter = '•'
	return keyInput
}

// wizardHasProvider reports whether at least one catalog account is
// configured (env or stored), or at least one custom provider is
// registered — a custom provider counts by existence alone, since its
// key is optional (see internal/customprovider's doc comment). The gate
// the wizard's provider step uses before letting "n" advance.
func (m *Model) wizardHasProvider() bool {
	for _, row := range m.accountRows() {
		if row.envSet || row.storedSet {
			return true
		}
	}
	return len(m.customProviders) > 0
}

// accountsRowCounts returns the cursor layout for the combined accounts
// list: staticCount catalog rows, then customCount registered custom
// providers, then exactly one "+ add" row — every cursor-position check
// in this file is written against these three numbers rather than
// hardcoding providercatalog.Accounts' length directly, so the custom
// rows and the add row are never forgotten in a bounds check.
func (m *Model) accountsRowCounts() (staticCount, customCount, addRow, total int) {
	staticCount = len(providercatalog.Accounts)
	customCount = len(m.customProviders)
	addRow = staticCount + customCount
	total = addRow + 1
	return
}

// currentCustomProvider returns the custom provider under the cursor, if
// any — nil when the cursor is on a static catalog row or the add row.
func (m *Model) currentCustomProvider() *customprovider.Provider {
	staticCount, customCount, _, _ := m.accountsRowCounts()
	if m.accountsCursor < staticCount || m.accountsCursor >= staticCount+customCount {
		return nil
	}
	return &m.customProviders[m.accountsCursor-staticCount]
}

// refreshCustomProviders reloads the cached list from customStore after
// an add/delete — nil-safe, same "best-effort" convention as every other
// local store here.
func (m *Model) refreshCustomProviders() {
	if m.customStore == nil {
		m.customProviders = nil
		return
	}
	m.customProviders = m.customStore.All()
}

func (m *Model) accountRows() []accountStatus {
	rows := make([]accountStatus, len(providercatalog.Accounts))
	for i, a := range providercatalog.Accounts {
		stored := m.credStore != nil && m.credStore.Get(a.EnvVar) != ""
		if !stored && m.credStore != nil {
			_, stored = m.credStore.GetOAuth(a.EnvVar)
		}
		rows[i] = accountStatus{
			envSet:    os.Getenv(a.EnvVar) != "",
			storedSet: stored,
		}
	}
	return rows
}

// accountByID finds a catalog account by ID, or nil if none matches —
// used to look up the right EnvVar/Label once an OAuth flow (started
// against an account ID) completes.
func accountByID(id string) *providercatalog.Account {
	for i := range providercatalog.Accounts {
		if providercatalog.Accounts[i].ID == id {
			return &providercatalog.Accounts[i]
		}
	}
	return nil
}

// renderAccounts draws the accounts screen: every known provider from
// providercatalog.Accounts, every registered internal/customprovider
// entry, an "+ add custom" row, and — depending on state — either the
// list+hints, a masked key-entry prompt, the custom-provider add form,
// or the in-progress OAuth flow.
func (m Model) renderAccounts() string {
	var b strings.Builder
	if m.wizardMode {
		b.WriteString(renderWizardHeader(3, "Providers") + "\n\n")
		b.WriteString(styleHint.Render("configure pelo menos um provedor para usar o gateway e os combos.") + "\n")
		b.WriteString(styleHint.Render("mais rápido: escolha OpenRouter e pressione \"o\" — autoriza no navegador, sem cartão, grátis.") + "\n\n")
	} else {
		b.WriteString(styleBody.Render("contas") + "\n\n")
	}

	staticCount, _, addRow, _ := m.accountsRowCounts()
	rows := m.accountRows()
	for i, a := range providercatalog.Accounts {
		status := styleHint.Render("— não configurado")
		switch {
		case rows[i].envSet:
			status = styleBadgeOK.Render("✓ definido (ambiente)")
		case rows[i].storedSet:
			status = styleBadgeOK.Render("✓ definido (salvo)")
		}
		dot := pingDot(m, a.EnvVar, rows[i].envSet || rows[i].storedSet)
		line := fmt.Sprintf("%s %-30s %s", dot, a.Label, status)
		if a.SupportsOAuth {
			line += "  " + styleHint.Render("(o: autorizar no navegador)")
		}
		if detail := pingDetail(m, a.EnvVar); detail != "" {
			line += "  " + styleHint.Render(detail)
		}
		if i == m.accountsCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}

	for i, cp := range m.customProviders {
		hasKey := m.credStore != nil && m.credStore.Get(cp.EnvVar) != ""
		status := styleBadgeOK.Render("✓ registrado (sem key)")
		if hasKey {
			status = styleBadgeOK.Render("✓ definido (salvo)")
		}
		dot := pingDot(m, cp.EnvVar, true) // existence alone means "configured" for a custom entry
		line := fmt.Sprintf("%s %-30s %s", dot, cp.Name, status)
		line += "  " + styleHint.Render(cp.BaseURL)
		if cp.Model != "" {
			line += "  " + styleHint.Render("modelo: "+cp.Model)
		}
		if detail := pingDetail(m, cp.EnvVar); detail != "" {
			line += "  " + styleHint.Render(detail)
		}
		rowIdx := staticCount + i
		if rowIdx == m.accountsCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}

	addLine := styleHint.Render("+ adicionar provedor customizado (URL + key opcional — servidor local/rede)")
	if addRow == m.accountsCursor {
		b.WriteString(styleYouTag.Render("▸ ") + addLine + "\n")
	} else {
		b.WriteString("  " + addLine + "\n")
	}
	b.WriteString("\n")

	if m.wizardMode {
		b.WriteString(wizardGatewayModeLine(rows, len(m.customProviders)) + "\n\n")
	}

	if m.accountsAddingCustom {
		return b.String() + m.renderCustomProviderForm()
	}

	if m.accountsEditing {
		b.WriteString(styleMeta.Render("cole a API key:") + "\n")
		b.WriteString(m.accountsKeyInput.View() + "\n\n")
		b.WriteString(styleHint.Render("enter salva · esc cancela"))
		return b.String()
	}

	if m.accountsOAuthPending {
		b.WriteString(styleMeta.Render("autorize no navegador que abriu — se não abriu, cole este link:") + "\n")
		b.WriteString(styleHint.Render(m.accountsOAuthURL) + "\n\n")
		b.WriteString(styleHint.Render("aguardando autorização… · esc cancela"))
		return b.String()
	}

	if m.accountsStatus != "" {
		b.WriteString(styleHint.Render(m.accountsStatus) + "\n\n")
	}

	hint := ""
	switch {
	case m.accountsCursor < staticCount:
		cur := providercatalog.Accounts[m.accountsCursor]
		if !cur.OAuthOnly {
			hint = "enter cola api key"
		}
		if cur.SupportsOAuth {
			if hint != "" {
				hint += " · "
			}
			hint += "o conecta via oauth"
		}
		if !m.wizardMode {
			hint += " · d remove chave salva"
		}
	case m.accountsCursor < addRow:
		hint = "enter define/atualiza a key"
		if !m.wizardMode {
			hint += " · d remove"
		}
	case m.accountsCursor == addRow:
		hint = "enter adiciona provedor customizado"
	}
	if m.wizardMode {
		hint += " · r verifica de novo"
		if m.wizardHasProvider() {
			hint += " · n continua"
		}
		if m.wizardWorkspaceLocked {
			hint += " · esc cancela"
		} else {
			hint += " · esc volta"
		}
	} else {
		hint += " · r verifica de novo · esc volta"
	}
	b.WriteString(styleHint.Render(hint))
	return b.String()
}

// customFormLabels are the fields of the "add custom provider" form, in
// cursor order.
var customFormLabels = []string{"nome", "url", "api key (opcional)", "modelo", "aceita tool calling? (s/n)"}

// renderCustomProviderForm draws the "+ add custom" form — a plain
// multi-field prompt in the same spirit as the single-field key-paste
// editor above it, just with more than one input.
func (m Model) renderCustomProviderForm() string {
	var b strings.Builder
	b.WriteString(styleMeta.Render("novo provedor customizado:") + "\n\n")
	for i, label := range customFormLabels {
		marker := "  "
		if i == m.customFormCursor {
			marker = styleYouTag.Render("▸ ")
		}
		b.WriteString(fmt.Sprintf("%s%-20s %s\n", marker, label, m.customFormInputs[i].View()))
	}
	if m.accountsStatus != "" {
		b.WriteString("\n" + styleHint.Render(m.accountsStatus))
	}
	b.WriteString("\n\n" + styleHint.Render("tab avança · shift+tab volta · enter salva · esc cancela"))
	return b.String()
}

// wizardGatewayModeLine reports how much real fallback the configured
// accounts actually buy — deliberately counting deduplicated accounts,
// not providercatalog.Providers entries: OpenRouter contributes 3 free-
// model routes from one account, and those must never be presented as 3
// independent upstreams.
func wizardGatewayModeLine(rows []accountStatus, customCount int) string {
	n := customCount
	for _, r := range rows {
		if r.envSet || r.storedSet {
			n++
		}
	}
	switch {
	case n == 0:
		return styleHint.Render("Gateway mode: —  (nenhum provedor configurado ainda)")
	case n == 1:
		return styleBadgeWarn.Render("Gateway mode: BASIC") + styleHint.Render("  · 1 upstream configurado — fallback multi-provider fica limitado")
	default:
		return styleBadgeOK.Render("Gateway mode: RESILIENT") + styleHint.Render(fmt.Sprintf("  · %d upstreams independentes", n))
	}
}

// pingDot renders one account's real, current status as a small colored
// dot — green (providerping.StatusOK), yellow (StatusDegraded: slow but
// working), red (StatusDown: no quota, invalid key, unreachable, or a
// server error), or a dim idle dot when there's nothing to check yet
// (no key configured) or it hasn't been checked this screen visit. Never
// simulated: the color reflects an actual request providerping.Ping just
// made, not a guess from whether a key merely looks present.
func pingDot(m Model, envVar string, configured bool) string {
	if !configured {
		return styleBadgeIdle.Render("○")
	}
	if m.accountsPinging {
		return styleBadgeWarn.Render("◉")
	}
	res, ok := m.accountsPings[envVar]
	if !ok {
		return styleBadgeIdle.Render("○")
	}
	switch res.Status {
	case providerping.StatusOK:
		return styleBadgeOK.Render("●")
	case providerping.StatusDegraded:
		return styleBadgeWarn.Render("●")
	case providerping.StatusDown:
		return styleBadgeBad.Render("●")
	default:
		return styleBadgeIdle.Render("○")
	}
}

// pingDetail is the short human-readable explanation shown next to a
// pinged account — the real reason a dot is red/yellow ("sem cota (429)",
// "chave inválida", "latência alta"), or the real latency for a clean
// green check. Empty when there's nothing to show yet.
func pingDetail(m Model, envVar string) string {
	res, ok := m.accountsPings[envVar]
	if !ok {
		return ""
	}
	if res.Detail != "" {
		return res.Detail
	}
	if res.Latency > 0 {
		return formatLatency(res.Latency.Milliseconds())
	}
	return ""
}

func (m Model) handleAccountsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.accountsAddingCustom {
		return m.handleCustomProviderFormKey(msg)
	}

	if m.accountsEditing {
		switch msg.String() {
		case "esc":
			m.accountsEditing = false
			return m, nil
		case "enter":
			key := strings.TrimSpace(m.accountsKeyInput.Value())
			m.accountsEditing = false
			if key == "" {
				return m, nil
			}
			envVar, label, ok := m.currentCredentialTarget()
			if !ok || m.credStore == nil {
				return m, nil
			}
			if err := m.credStore.Set(envVar, key); err != nil {
				m.accountsStatus = "erro ao salvar: " + err.Error()
				return m, nil
			}
			m.accountsStatus = label + ": chave salva — reinicie o kram pra usar."
			if m.wizardMode {
				// Validate immediately rather than waiting for a manual
				// "r" — the wizard's whole point is real feedback as the
				// user goes, not a key that might silently fail later.
				m.accountsPinging = true
				return m, pingAccountsCmd(m.credStore, m.customProviders)
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.accountsKeyInput, cmd = m.accountsKeyInput.Update(msg)
		return m, cmd
	}

	if m.accountsOAuthPending {
		if msg.String() == "esc" {
			m.accountsOAuthPending = false
			if m.accountsOAuthCancel != nil {
				m.accountsOAuthCancel()
				m.accountsOAuthCancel = nil
			}
		}
		return m, nil
	}

	staticCount, _, addRow, total := m.accountsRowCounts()

	switch msg.String() {
	case "esc":
		if m.wizardMode {
			if m.wizardWorkspaceLocked {
				m.wizardCancelled = true
				return m, tea.Quit
			}
			m.phase = phaseWizardProjects
			return m, nil
		}
		m.phase = phasePicker
		return m, nil
	case "up":
		if m.accountsCursor > 0 {
			m.accountsCursor--
		}
	case "down":
		if m.accountsCursor < total-1 {
			m.accountsCursor++
		}
	case "enter":
		switch {
		case m.accountsCursor < staticCount:
			if providercatalog.Accounts[m.accountsCursor].OAuthOnly {
				return m, nil // no key to paste — "o" is this account's only path
			}
			m.accountsEditing = true
			m.accountsStatus = ""
			m.accountsKeyInput.SetValue("")
			m.accountsKeyInput.Focus()
			return m, textinput.Blink
		case m.accountsCursor < addRow:
			// An existing custom provider — enter sets/updates its
			// (optional) key, reusing the same masked-input flow. Editing
			// name/URL/model in place is out of scope: delete and re-add
			// covers it.
			m.accountsEditing = true
			m.accountsStatus = ""
			m.accountsKeyInput.SetValue("")
			m.accountsKeyInput.Focus()
			return m, textinput.Blink
		default: // addRow
			m.accountsAddingCustom = true
			m.accountsStatus = ""
			m.customFormInputs = newCustomProviderFormInputs()
			m.customFormCursor = 0
			m.customFormInputs[0].Focus()
			return m, textinput.Blink
		}
	case "d":
		switch {
		case m.accountsCursor < staticCount:
			if m.wizardMode {
				return m, nil // catalog keys are always freshly entered during setup — nothing to undo yet
			}
			acct := providercatalog.Accounts[m.accountsCursor]
			if m.credStore != nil {
				_ = m.credStore.Delete(acct.EnvVar)
				_ = m.credStore.DeleteOAuth(acct.EnvVar)
				m.accountsStatus = acct.Label + ": credencial removida."
			}
		case m.accountsCursor < addRow:
			// Unlike catalog credentials, a custom provider can be deleted
			// during setup too — a mistyped URL/name registered mid-wizard
			// needs a way to be fixed before finishing, not just after.
			if cp := m.currentCustomProvider(); cp != nil && m.customStore != nil {
				name := cp.Name
				_ = m.customStore.Delete(cp.ID)
				if m.credStore != nil {
					_ = m.credStore.Delete(cp.EnvVar)
				}
				m.refreshCustomProviders()
				if _, _, newAddRow, _ := m.accountsRowCounts(); m.accountsCursor > newAddRow {
					m.accountsCursor = newAddRow
				}
				m.accountsStatus = name + ": provedor removido."
			}
		}
	case "o":
		if m.accountsCursor < staticCount {
			acct := providercatalog.Accounts[m.accountsCursor]
			if acct.SupportsOAuth {
				m.accountsOAuthPending = true
				m.accountsStatus = ""
				return m, startOAuthCmd(acct.ID)
			}
		}
	case "r":
		m.accountsPinging = true
		m.accountsStatus = ""
		return m, pingAccountsCmd(m.credStore, m.customProviders)
	case "n":
		if m.wizardMode && m.wizardHasProvider() {
			m.phase = phaseWizardRouting
			m.wizardStep = 4
			return m, nil
		}
		if m.wizardMode {
			m.accountsStatus = "configure ao menos um provedor antes de continuar."
		}
	}
	return m, nil
}

// currentCredentialTarget resolves which envVar/label the masked
// key-paste editor (accountsEditing) is currently acting on — a static
// catalog account or an existing custom provider, whichever the cursor
// is on. ok is false when the cursor is on the "+ add" row, which has no
// key of its own to edit (use "enter" there to open the add form
// instead).
func (m Model) currentCredentialTarget() (envVar, label string, ok bool) {
	staticCount, _, addRow, _ := m.accountsRowCounts()
	switch {
	case m.accountsCursor < staticCount:
		acct := providercatalog.Accounts[m.accountsCursor]
		return acct.EnvVar, acct.Label, true
	case m.accountsCursor < addRow:
		if cp := m.currentCustomProvider(); cp != nil {
			return cp.EnvVar, cp.Name, true
		}
	}
	return "", "", false
}

// newCustomProviderFormInputs builds the four fields of the "add custom
// provider" form, in the same order as customFormLabels.
func newCustomProviderFormInputs() []textinput.Model {
	name := textinput.New()
	name.Placeholder = "Meu Servidor"
	name.CharLimit = 80
	name.Prompt = "› "

	url := textinput.New()
	url.Placeholder = "http://192.168.1.50:8080/v1"
	url.CharLimit = 300
	url.Prompt = "› "

	key := newAccountsKeyInput()

	model := textinput.New()
	model.Placeholder = "llama-3, gpt-oss-20b, etc."
	model.CharLimit = 200
	model.Prompt = "› "

	// SupportsTools defaults to true (most OpenAI-compat local servers —
	// llama.cpp, LM Studio, vLLM, Ollama — do support tool calling), so
	// this field starts pre-filled "s" (sim) rather than empty — a user
	// who wants the common case just tabs past it.
	tools := textinput.New()
	tools.SetValue("s")
	tools.CharLimit = 3
	tools.Prompt = "› "

	return []textinput.Model{name, url, key, model, tools}
}

// parseSupportsToolsInput reads the custom-provider form's "aceita tool
// calling?" field leniently — empty or anything starting with "s"/"y"
// (sim/yes) is true, anything starting with "n" (não/no) is false;
// unrecognized input also defaults true, matching the field's own
// pre-filled default rather than silently disabling tool calling on a
// typo.
func parseSupportsToolsInput(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "n") {
		return false
	}
	return true
}

// handleCustomProviderFormKey drives the "+ add custom" form: tab/shift+tab
// move focus, esc cancels, enter validates and submits regardless of
// which field currently has focus (the same "enter always finishes"
// convention every other single-field prompt in this screen already
// uses), anything else is forwarded to the focused input.
func (m Model) handleCustomProviderFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.accountsAddingCustom = false
		m.accountsStatus = ""
		return m, nil
	case "tab":
		m.customFormInputs[m.customFormCursor].Blur()
		m.customFormCursor = (m.customFormCursor + 1) % len(m.customFormInputs)
		m.customFormInputs[m.customFormCursor].Focus()
		return m, textinput.Blink
	case "shift+tab":
		m.customFormInputs[m.customFormCursor].Blur()
		m.customFormCursor = (m.customFormCursor - 1 + len(m.customFormInputs)) % len(m.customFormInputs)
		m.customFormInputs[m.customFormCursor].Focus()
		return m, textinput.Blink
	case "enter":
		name := strings.TrimSpace(m.customFormInputs[0].Value())
		url := strings.TrimSpace(m.customFormInputs[1].Value())
		key := strings.TrimSpace(m.customFormInputs[2].Value())
		model := strings.TrimSpace(m.customFormInputs[3].Value())
		supportsTools := parseSupportsToolsInput(m.customFormInputs[4].Value())

		if m.customStore == nil {
			m.accountsStatus = "erro: armazenamento local indisponível."
			return m, nil
		}
		cp, err := m.customStore.Add(name, url, model, supportsTools)
		if err != nil {
			m.accountsStatus = err.Error()
			return m, nil
		}
		if key != "" && m.credStore != nil {
			if err := m.credStore.Set(cp.EnvVar, key); err != nil {
				m.accountsStatus = "provedor salvo, mas erro ao salvar a key: " + err.Error()
			}
		}
		m.accountsAddingCustom = false
		m.refreshCustomProviders()
		_, customCount, _, _ := m.accountsRowCounts()
		staticCount := len(providercatalog.Accounts)
		m.accountsCursor = staticCount + customCount - 1 // land on the row just added
		m.accountsStatus = cp.Name + ": provedor adicionado — reinicie o kram pra usar."
		if m.wizardMode {
			m.accountsPinging = true
			return m, pingAccountsCmd(m.credStore, m.customProviders)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.customFormInputs[m.customFormCursor], cmd = m.customFormInputs[m.customFormCursor].Update(msg)
	return m, cmd
}

// oauthURLMsg reports that the local callback listener is up and an
// authorization URL is ready — the browser opens (best-effort) and the
// screen shows the URL as a fallback the instant this lands, before the
// (potentially long) wait for the user to actually click through.
//
// Exactly one of waitPermanent/waitRefreshable is set, matching the two
// shapes internal/oauthflow's Authorize functions return: OpenRouter and
// Anthropic both exchange for a permanent API key (waitPermanent) — for
// Anthropic that's one extra create-key call tucked inside its own
// Authorize function, not something this file needs to know about — while
// only OpenAI's ChatGPT login exchanges for a refreshable oauthflow.Token
// (waitRefreshable), since that one has no equivalent permanent-key step.
// See internal/oauthflow/anthropic.go's doc comment for why.
type oauthURLMsg struct {
	acctID          string
	url             string
	waitPermanent   func(ctx context.Context) (string, error)
	waitRefreshable func(ctx context.Context) (oauthflow.Token, error)
	err             error
}

// startOAuthCmd dispatches to the right catalog account's OAuth flow.
// Only accounts with SupportsOAuth true ever reach this (see the "o" key
// handler above); a default case exists purely so an unmapped ID reports
// a real error instead of panicking on a nil wait function.
func startOAuthCmd(acctID string) tea.Cmd {
	return func() tea.Msg {
		switch acctID {
		case "openrouter":
			url, wait, err := oauthflow.OpenRouterAuthorize()
			return oauthURLMsg{acctID: acctID, url: url, waitPermanent: wait, err: err}
		case "anthropic":
			url, wait, err := oauthflow.AnthropicAuthorize()
			return oauthURLMsg{acctID: acctID, url: url, waitPermanent: wait, err: err}
		case "openai-chatgpt":
			url, wait, err := oauthflow.OpenAIAuthorize()
			return oauthURLMsg{acctID: acctID, url: url, waitRefreshable: wait, err: err}
		default:
			return oauthURLMsg{acctID: acctID, err: fmt.Errorf("no oauth flow implemented for %q", acctID)}
		}
	}
}

// oauthResultMsg carries whichever shape of credential the flow produced
// — refreshable is the discriminator model.go's handler switches on,
// since a zero-value Token and a zero-value string key are both
// indistinguishable from "not set" on their own.
type oauthResultMsg struct {
	acctID      string
	key         string
	token       oauthflow.Token
	refreshable bool
	err         error
}

func waitOAuthPermanentCmd(ctx context.Context, acctID string, wait func(ctx context.Context) (string, error)) tea.Cmd {
	return func() tea.Msg {
		key, err := wait(ctx)
		return oauthResultMsg{acctID: acctID, key: key, err: err}
	}
}

func waitOAuthRefreshableCmd(ctx context.Context, acctID string, wait func(ctx context.Context) (oauthflow.Token, error)) tea.Cmd {
	return func() tea.Msg {
		tok, err := wait(ctx)
		return oauthResultMsg{acctID: acctID, token: tok, refreshable: true, err: err}
	}
}

// saveOAuthResult persists whichever credential shape msg carries against
// the right catalog account and reports a real status line — shared by
// model.go's oauthResultMsg handler so the Set-vs-SetOAuth branch only
// lives in one place.
func saveOAuthResult(credStore *credentials.Store, msg oauthResultMsg) (status string, err error) {
	acct := accountByID(msg.acctID)
	if acct == nil {
		return "", fmt.Errorf("unknown account %q", msg.acctID)
	}
	if msg.refreshable {
		if err := credStore.SetOAuth(acct.EnvVar, credentials.OAuthToken{
			Access: msg.token.Access, Refresh: msg.token.Refresh, ExpiresAt: msg.token.ExpiresAt,
		}); err != nil {
			return "", err
		}
	} else {
		if err := credStore.Set(acct.EnvVar, msg.key); err != nil {
			return "", err
		}
	}
	return acct.Label + ": conectado — reinicie o kram pra usar.", nil
}

// openBrowser is best-effort — the authorization URL is always shown on
// screen too (renderAccounts), so a platform where this silently fails
// (headless, unusual $PATH, sandboxed) still leaves the user a working
// path forward: copy the link themselves.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
