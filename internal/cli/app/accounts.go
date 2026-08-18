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

func (m *Model) accountRows() []accountStatus {
	rows := make([]accountStatus, len(providercatalog.Accounts))
	for i, a := range providercatalog.Accounts {
		rows[i] = accountStatus{
			envSet:    os.Getenv(a.EnvVar) != "",
			storedSet: m.credStore != nil && m.credStore.Get(a.EnvVar) != "",
		}
	}
	return rows
}

// renderAccounts draws the accounts screen: every known provider from
// providercatalog.Accounts, its current status, and — depending on
// state — either the list+hints, a masked key-entry prompt, or the
// in-progress OAuth flow.
func (m Model) renderAccounts() string {
	var b strings.Builder
	b.WriteString(styleBody.Render("contas") + "\n\n")

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
		if detail := pingDetail(m, a.EnvVar); detail != "" {
			line += "  " + styleHint.Render(detail)
		}
		if i == m.accountsCursor {
			b.WriteString(styleYouTag.Render("▸ ") + styleBody.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n")

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

	hint := "enter cola api key"
	if m.accountsCursor < len(providercatalog.Accounts) && providercatalog.Accounts[m.accountsCursor].SupportsOAuth {
		hint += " · o conecta via oauth"
	}
	hint += " · d remove chave salva · r verifica de novo · esc volta"
	b.WriteString(styleHint.Render(hint))
	return b.String()
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
			acct := providercatalog.Accounts[m.accountsCursor]
			if m.credStore != nil {
				if err := m.credStore.Set(acct.EnvVar, key); err != nil {
					m.accountsStatus = "erro ao salvar: " + err.Error()
				} else {
					m.accountsStatus = acct.Label + ": chave salva — reinicie o kram pra usar."
				}
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
		}
		return m, nil
	}

	switch msg.String() {
	case "esc":
		m.phase = phasePicker
		return m, nil
	case "up":
		if m.accountsCursor > 0 {
			m.accountsCursor--
		}
	case "down":
		if m.accountsCursor < len(providercatalog.Accounts)-1 {
			m.accountsCursor++
		}
	case "enter":
		m.accountsEditing = true
		m.accountsStatus = ""
		m.accountsKeyInput.SetValue("")
		m.accountsKeyInput.Focus()
		return m, textinput.Blink
	case "d":
		acct := providercatalog.Accounts[m.accountsCursor]
		if m.credStore != nil {
			_ = m.credStore.Delete(acct.EnvVar)
			m.accountsStatus = acct.Label + ": chave salva removida."
		}
	case "o":
		acct := providercatalog.Accounts[m.accountsCursor]
		if acct.SupportsOAuth {
			m.accountsOAuthPending = true
			m.accountsStatus = ""
			return m, startOpenRouterOAuthCmd()
		}
	case "r":
		m.accountsPinging = true
		m.accountsStatus = ""
		return m, pingAccountsCmd(m.credStore)
	}
	return m, nil
}

// oauthURLMsg reports that the local callback listener is up and an
// authorization URL is ready — the browser opens (best-effort) and the
// screen shows the URL as a fallback the instant this lands, before the
// (potentially long) wait for the user to actually click through.
type oauthURLMsg struct {
	url  string
	wait func(ctx context.Context) (string, error)
	err  error
}

func startOpenRouterOAuthCmd() tea.Cmd {
	return func() tea.Msg {
		url, wait, err := oauthflow.OpenRouterAuthorize()
		return oauthURLMsg{url: url, wait: wait, err: err}
	}
}

type oauthResultMsg struct {
	key string
	err error
}

func waitOpenRouterOAuthCmd(wait func(ctx context.Context) (string, error)) tea.Cmd {
	return func() tea.Msg {
		key, err := wait(context.Background())
		return oauthResultMsg{key: key, err: err}
	}
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
