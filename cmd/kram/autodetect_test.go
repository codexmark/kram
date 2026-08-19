package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/kramhome"
	"github.com/codexmark/kram/internal/providercatalog"
)

// isolateReconcileTest points customprovider.Store at a fresh temp dir
// and clears every catalog provider's env var — this machine's real
// shell environment may have real keys exported (e.g. a developer's own
// OPENROUTER_API_KEY), and reconcileLiveProviders reads os.Getenv
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
	cp, err := customStore.Add("lab", "http://127.0.0.1:9999/v1", "some-model", true)
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

	got := reconcileLiveProviders(cfg, nil, slog.New(slog.DiscardHandler))

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

	got := reconcileLiveProviders(cfg, nil, slog.New(slog.DiscardHandler))

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

	got := reconcileLiveProviders(cfg, nil, slog.New(slog.DiscardHandler))

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
// customProviderConfig (shared by detectGatewayConfig and
// reconcileLiveProviders) must skip it rather than build a
// ProviderConfig that would forward a bogus model name upstream.
func TestCustomProviderConfigSkipsEmptyModel(t *testing.T) {
	_, ok := customProviderConfig(customprovider.Provider{ID: "legacy", Name: "legacy", BaseURL: "http://x", Model: ""})
	if ok {
		t.Error("expected a custom provider with no model configured to be skipped")
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
	if _, err := customStore.Add("legacy", "http://x", "placeholder", true); err != nil {
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
	got := reconcileLiveProviders(cfg, nil, slog.New(slog.DiscardHandler))

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
