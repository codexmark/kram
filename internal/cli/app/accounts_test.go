package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/providercatalog"
)

// isolateAccountsTest points every local store this package touches at a
// fresh temp dir, same isolation pattern internal/credentials'/
// internal/customprovider's own tests already use.
func isolateAccountsTest(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// TestDeleteKeyRemovesStoredCatalogCredential exercises "d" on a static
// catalog row (not env-sourced, not wizard mode) — the base case this
// screen has always supported.
func TestDeleteKeyRemovesStoredCatalogCredential(t *testing.T) {
	isolateAccountsTest(t)
	credStore, err := credentials.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Anthropic is providercatalog.Accounts[0].
	acct := providercatalog.Accounts[0]
	if err := credStore.Set(acct.EnvVar, "sk-test"); err != nil {
		t.Fatal(err)
	}

	m := Model{credStore: credStore, accountsCursor: 0}
	next, _ := m.handleAccountsKey(keyMsg("d"))
	got := next.(Model)

	if credStore.Get(acct.EnvVar) != "" {
		t.Errorf("expected %s's stored key to be deleted, still got a value", acct.Label)
	}
	if got.accountsStatus == "" {
		t.Error("expected a status message confirming the deletion")
	}
}

// TestDeleteKeyOnEnvSourcedAccountIsANoOp documents real, existing
// behavior rather than a bug: a key coming from a real shell env var was
// never stored by kram in the first place, so credStore.Delete has
// nothing to remove — "d" can't un-set a real environment variable, by
// design (see accountStatus's doc comment). If a user presses "d" on a
// row showing "definido (ambiente)", the row correctly still shows
// configured afterward.
func TestDeleteKeyOnEnvSourcedAccountIsANoOp(t *testing.T) {
	isolateAccountsTest(t)
	credStore, _ := credentials.Load()
	acct := providercatalog.Accounts[0]
	t.Setenv(acct.EnvVar, "sk-from-shell")

	m := Model{credStore: credStore, accountsCursor: 0}
	next, _ := m.handleAccountsKey(keyMsg("d"))
	got := next.(Model)

	rows := got.accountRows()
	if !rows[0].envSet {
		t.Error("an env-sourced credential must still read as set after 'd' — it was never kram's to delete")
	}
}

// TestDeleteKeyRemovesCustomProvider is the scenario the user reported
// trouble with directly: cursor on a registered custom provider, "d"
// should remove it from both customStore and credStore.
func TestDeleteKeyRemovesCustomProvider(t *testing.T) {
	isolateAccountsTest(t)
	credStore, err := credentials.Load()
	if err != nil {
		t.Fatal(err)
	}
	customStore, err := customprovider.Load()
	if err != nil {
		t.Fatal(err)
	}
	cp, err := customStore.Add("lab", "http://192.168.0.4:20128/v1", "omni.codexmark")
	if err != nil {
		t.Fatal(err)
	}
	if err := credStore.Set(cp.EnvVar, "sk-lab-key"); err != nil {
		t.Fatal(err)
	}

	staticCount := len(providercatalog.Accounts)
	m := Model{
		credStore: credStore, customStore: customStore, customProviders: customStore.All(),
		accountsCursor: staticCount, // the first (only) custom row
	}
	if cp := m.currentCustomProvider(); cp == nil {
		t.Fatal("test setup bug: cursor should already be on the custom provider row")
	}

	next, _ := m.handleAccountsKey(keyMsg("d"))
	got := next.(Model)

	if len(got.customProviders) != 0 {
		t.Errorf("expected the custom provider to be removed from the cached list, got %d entries", len(got.customProviders))
	}
	if len(customStore.All()) != 0 {
		t.Errorf("expected the custom provider to be removed from the store, got %d entries", len(customStore.All()))
	}
	if credStore.Get(cp.EnvVar) != "" {
		t.Error("expected the custom provider's stored key to be deleted too")
	}
	if got.accountsStatus == "" {
		t.Error("expected a status message confirming the deletion")
	}
}

// TestDeleteKeyOnAddRowIsANoOp confirms pressing "d" while the cursor is
// on the "+ add custom" row does nothing destructive — there's nothing
// there to delete.
func TestDeleteKeyOnAddRowIsANoOp(t *testing.T) {
	isolateAccountsTest(t)
	credStore, _ := credentials.Load()
	customStore, _ := customprovider.Load()
	cp, _ := customStore.Add("lab", "http://x", "")
	_ = credStore.Set(cp.EnvVar, "sk-lab")

	staticCount := len(providercatalog.Accounts)
	_, customCount, addRow, _ := (&Model{customProviders: customStore.All()}).accountsRowCounts()
	if addRow != staticCount+customCount {
		t.Fatalf("test sanity check failed: addRow=%d, want %d", addRow, staticCount+customCount)
	}

	m := Model{
		credStore: credStore, customStore: customStore, customProviders: customStore.All(),
		accountsCursor: addRow,
	}
	next, _ := m.handleAccountsKey(keyMsg("d"))
	got := next.(Model)

	if len(got.customProviders) != 1 {
		t.Errorf("the add row has nothing to delete — expected the one existing provider to survive, got %d", len(got.customProviders))
	}
}

// TestDeleteKeyOnCatalogRowBlockedDuringWizardSetup documents the
// deliberate restriction that still applies to catalog credentials: "d"
// is a no-op there in wizardMode, since a fresh setup run has nothing to
// undo for those.
func TestDeleteKeyOnCatalogRowBlockedDuringWizardSetup(t *testing.T) {
	isolateAccountsTest(t)
	credStore, _ := credentials.Load()
	acct := providercatalog.Accounts[0]
	_ = credStore.Set(acct.EnvVar, "sk-test")

	m := Model{credStore: credStore, accountsCursor: 0, wizardMode: true}
	next, _ := m.handleAccountsKey(keyMsg("d"))
	got := next.(Model)

	if credStore.Get(acct.EnvVar) == "" {
		t.Error("wizardMode should still block catalog-credential deletion")
	}
	_ = got
}

// TestDeleteKeyOnCustomProviderWorksDuringWizardSetup is the opposite of
// the catalog case above, and deliberately so: a custom provider can be
// registered mid-wizard (see the Providers step), so the user needs a way
// to fix a mistyped one — delete and re-add — before finishing setup,
// not only after.
func TestDeleteKeyOnCustomProviderWorksDuringWizardSetup(t *testing.T) {
	isolateAccountsTest(t)
	credStore, _ := credentials.Load()
	customStore, _ := customprovider.Load()
	cp, _ := customStore.Add("lab", "http://x", "")
	_ = credStore.Set(cp.EnvVar, "sk-lab")

	staticCount := len(providercatalog.Accounts)
	m := Model{
		credStore: credStore, customStore: customStore, customProviders: customStore.All(),
		accountsCursor: staticCount, wizardMode: true,
	}
	next, _ := m.handleAccountsKey(keyMsg("d"))
	got := next.(Model)

	if len(got.customProviders) != 0 {
		t.Error("a custom provider added mid-wizard should be deletable mid-wizard too")
	}
}
