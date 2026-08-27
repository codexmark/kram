package gatewayconfig

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/kramhome"
	"github.com/codexmark/kram/internal/providercatalog"
)

func TestLoadStoredCredentialsAndAutodetectionStrategies(t *testing.T) {
	isolateReconcileTest(t)
	store, err := credentials.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("ANTHROPIC_API_KEY", "stored-key"); err != nil {
		t.Fatal(err)
	}
	LoadStoredCredentials()
	if got := os.Getenv("ANTHROPIC_API_KEY"); got != "stored-key" {
		t.Fatalf("loaded key = %q", got)
	}
	// A single detected provider (anthropic, via the stored key) yields a
	// single-provider combo: strategy left empty (routing is moot).
	cfg, err := Detect("", nil, slog.New(slog.DiscardHandler))
	if err != nil || len(cfg.Combos[0].Providers) != 1 || cfg.Combos[0].Strategy != "" {
		t.Fatalf("single-provider strategy = %q (providers %v) err=%v", cfg.Combos[0].Strategy, cfg.Combos[0].Providers, err)
	}
}

// TestAutoStrategyByProviderCount pins the count-based default: one provider
// is moot ("", a trivial single-candidate no-op), two or more default to
// smart (auto-aligns on health/reliability).
func TestAutoStrategyByProviderCount(t *testing.T) {
	if autoStrategy(0) != "" || autoStrategy(1) != "" {
		t.Errorf("<=1 provider should give empty strategy, got %q/%q", autoStrategy(0), autoStrategy(1))
	}
	if autoStrategy(2) != "smart" || autoStrategy(5) != "smart" {
		t.Errorf(">=2 providers should give smart, got %q/%q", autoStrategy(2), autoStrategy(5))
	}
}

// TestDetectMultiProviderDefaultsToSmart confirms that once two providers
// are detected, the auto-built combo routes with smart.
func TestDetectMultiProviderDefaultsToSmart(t *testing.T) {
	isolateReconcileTest(t)
	t.Setenv("ANTHROPIC_API_KEY", "k1")
	t.Setenv("OPENAI_API_KEY", "k2")
	cfg, err := Detect("", nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Combos[0].Providers) < 2 || cfg.Combos[0].Strategy != "smart" {
		t.Fatalf("multi-provider strategy = %q (providers %v), want smart", cfg.Combos[0].Strategy, cfg.Combos[0].Providers)
	}
}

func TestCatalogProviderConfigSupportsOAuthAndMissingCredentials(t *testing.T) {
	isolateReconcileTest(t)
	var oauthProvider providercatalog.Provider
	for _, provider := range providercatalog.Providers {
		if provider.SupportsOAuth {
			oauthProvider = provider
			break
		}
	}
	if oauthProvider.ID == "" {
		t.Skip("catalog currently has no OAuth provider")
	}
	if _, ok := catalogProviderConfig(oauthProvider, nil); ok {
		t.Fatal("provider without any credential was accepted")
	}
	store, err := credentials.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOAuth(oauthProvider.EnvVar, credentials.OAuthToken{Access: "access", Refresh: "refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, ok := catalogProviderConfig(oauthProvider, store)
	if !ok || got.AuthMode != "oauth" {
		t.Fatalf("OAuth provider = %+v ok=%v", got, ok)
	}
}

func TestDetectGatewayConfigErrorsWithoutProviderAndIncludesCustom(t *testing.T) {
	isolateReconcileTest(t)
	if _, err := Detect("", nil, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "no LLM provider") {
		t.Fatalf("empty autodetection error = %v", err)
	}
	store, err := customprovider.Load()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := store.Add("local", "http://127.0.0.1:9999/v1", "local-model", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Detect("smart", nil, slog.New(slog.DiscardHandler))
	if err != nil || len(cfg.Providers) != 1 || cfg.Providers[0].ID != "custom-"+provider.ID || cfg.Combos[0].Strategy != "smart" {
		t.Fatalf("custom autodetection = %+v err=%v", cfg, err)
	}
}

// TestDetectFreeCatalogProviderStrategyByCount replaces the old
// "free tier ⇒ round-robin" rule: routing now keys on provider count, not
// tier. Setting a free provider's env var detects however many catalog
// entries share it, and the strategy follows that count (≥2 → smart,
// 1 → "" / single).
func TestDetectFreeCatalogProviderStrategyByCount(t *testing.T) {
	isolateReconcileTest(t)
	var free providercatalog.Provider
	for _, provider := range providercatalog.Providers {
		if provider.FreeTier {
			free = provider
			break
		}
	}
	if free.ID == "" {
		t.Skip("catalog has no free-tier provider")
	}
	t.Setenv(free.EnvVar, "test-key")
	cfg, err := Detect("", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	if len(cfg.Combos[0].Providers) >= 2 {
		want = "smart"
	}
	if cfg.Combos[0].Strategy != want {
		t.Fatalf("strategy = %q for %d providers, want %q", cfg.Combos[0].Strategy, len(cfg.Combos[0].Providers), want)
	}
}

// isolateReconcileTest points customprovider.Store at a fresh temp dir
// and clears every catalog provider's env var — this machine's real
// shell environment may have real keys exported (e.g. a developer's own
// OPENROUTER_API_KEY), and Reconcile reads os.Getenv
// directly, so without this a test asserting an exact provider count
// would be at the mercy of whatever happens to be exported outside the
// test.
func isolateReconcileTest(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, p := range providercatalog.Providers {
		t.Setenv(p.EnvVar, "")
	}
}

// TestReconcileLiveProvidersAddsNewCustomProvider is the regression test
// for the config split-brain bug: a custom provider registered after
// config.yaml already existed must actually become reachable on the
// next boot, not stay invisible until the file is hand-edited.
func TestReconcileLiveProvidersAddsNewCustomProvider(t *testing.T) {
	isolateReconcileTest(t)
	customStore, err := customprovider.Load()
	if err != nil {
		t.Fatal(err)
	}
	cp, err := customStore.Add("lab", "http://127.0.0.1:9999/v1", "some-model", true, 0)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Host: "127.0.0.1", Port: 20128,
		Providers: []config.ProviderConfig{{ID: "anthropic", Kind: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"}},
		Combos: []config.ComboConfig{
			{ID: "default", Strategy: "priority", Providers: []string{"anthropic"}},
		},
		DefaultCombo: "default",
	}

	got := Reconcile(cfg, nil, slog.New(slog.DiscardHandler))

	if len(got.Providers) != 2 {
		t.Fatalf("expected 2 providers after reconciliation, got %d: %+v", len(got.Providers), got.Providers)
	}
	wantID := "custom-" + cp.ID
	found := false
	for _, pc := range got.Providers {
		if pc.ID == wantID {
			found = true
			if pc.BaseURL != cp.BaseURL {
				t.Errorf("reconciled provider BaseURL = %q, want %q", pc.BaseURL, cp.BaseURL)
			}
		}
	}
	if !found {
		t.Fatalf("expected reconciled Providers to contain %q, got %+v", wantID, got.Providers)
	}

	if len(got.Combos) != 1 {
		t.Fatalf("expected 1 combo, got %d", len(got.Combos))
	}
	wantCombo := []string{"anthropic", wantID}
	if gotIDs := got.Combos[0].Providers; !stringSliceEqual(gotIDs, wantCombo) {
		t.Errorf("default combo providers = %v, want %v (new provider appended at the end)", gotIDs, wantCombo)
	}

	// The original cfg (and everything about it besides Providers/Combos)
	// must be untouched — strategy, response gate, other fields.
	if got.Combos[0].Strategy != "priority" {
		t.Errorf("combo strategy changed unexpectedly: %q", got.Combos[0].Strategy)
	}
	if got.Host != cfg.Host || got.Port != cfg.Port {
		t.Errorf("unrelated top-level fields changed: got Host=%q Port=%d", got.Host, got.Port)
	}
}

// TestReconcileLiveProvidersNoOpWhenNothingNew confirms the common case
// (nothing changed since the file was written) returns cfg unchanged —
// no spurious copy, no reordering.
func TestReconcileLiveProvidersNoOpWhenNothingNew(t *testing.T) {
	isolateReconcileTest(t)

	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{ID: "anthropic", Kind: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"anthropic"}}},
		DefaultCombo: "default",
	}

	got := Reconcile(cfg, nil, slog.New(slog.DiscardHandler))

	if got != cfg {
		t.Error("expected the exact same *config.Config pointer back when there's nothing new to reconcile")
	}
}

// TestReconcileLiveProvidersAddsCatalogProviderWithNewCredential covers
// the catalog-provider half of the split-brain bug: an account
// connected via the Accounts UI after config.yaml existed (a real env
// var, in this test) must also get reconciled in.
func TestReconcileLiveProvidersAddsCatalogProviderWithNewCredential(t *testing.T) {
	isolateReconcileTest(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")

	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{ID: "openai", Kind: "openai-compat", APIKeyEnv: "OPENAI_API_KEY"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"openai"}}},
		DefaultCombo: "default",
	}

	got := Reconcile(cfg, nil, slog.New(slog.DiscardHandler))

	found := false
	for _, pc := range got.Providers {
		if pc.ID == "anthropic" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected anthropic to be reconciled in now that its env var is set, got %+v", got.Providers)
	}
}

// TestCustomProviderConfigSkipsEmptyModel is the defense-in-depth case:
// customprovider.Store.Add rejects an empty model today, but an entry
// saved before that validation existed could still have one on disk —
// customProviderConfig (shared by Detect and
// Reconcile) must skip it rather than build a
// ProviderConfig that would forward a bogus model name upstream.
func TestCustomProviderConfigSkipsEmptyModel(t *testing.T) {
	_, ok := customProviderConfig(customprovider.Provider{ID: "legacy", Name: "legacy", BaseURL: "http://x", Model: ""})
	if ok {
		t.Error("expected a custom provider with no model configured to be skipped")
	}
}

// TestCustomProviderConfigPropagatesContextWindow confirms a custom
// provider's declared window reaches the built config.ProviderConfig, so a
// local model's real window can feed the compaction budget.
func TestCustomProviderConfigPropagatesContextWindow(t *testing.T) {
	pc, ok := customProviderConfig(customprovider.Provider{ID: "lab", Name: "lab", BaseURL: "http://x", Model: "qwen", ContextWindow: 32768})
	if !ok {
		t.Fatal("expected a valid custom provider to build")
	}
	if pc.ContextWindow != 32768 {
		t.Errorf("ContextWindow = %d, want 32768", pc.ContextWindow)
	}
}

// TestReconcileLiveProvidersSkipsCustomProviderWithNoModel confirms the
// same defense-in-depth skip applies through the reconciliation path,
// not just the fresh-build path — with a warning, not a fatal error.
func TestReconcileLiveProvidersSkipsCustomProviderWithNoModel(t *testing.T) {
	isolateReconcileTest(t)
	customStore, err := customprovider.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Add a valid entry, then hand-corrupt it to simulate a pre-validation
	// legacy record with no model — Store.Add itself no longer allows
	// creating one.
	if _, err := customStore.Add("legacy", "http://x", "placeholder", true, 0); err != nil {
		t.Fatal(err)
	}
	entries := customStore.All()
	entries[0].Model = ""
	corrupted, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	path, err := kramhome.Path("custom_providers.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, corrupted, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{ID: "anthropic", Kind: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"anthropic"}}},
		DefaultCombo: "default",
	}
	got := Reconcile(cfg, nil, slog.New(slog.DiscardHandler))

	if len(got.Providers) != 1 {
		t.Errorf("expected the model-less custom provider to be skipped, got %d providers: %+v", len(got.Providers), got.Providers)
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
