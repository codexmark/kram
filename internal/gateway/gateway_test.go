package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/provider"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestSanitizeCombosForBuiltProviders_DropsUnbuiltProviderFromCombo covers
// the common case: one provider out of several in a combo failed to
// build, the combo keeps running with whichever providers remain.
func TestSanitizeCombosForBuiltProviders_DropsUnbuiltProviderFromCombo(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "a"}, {ID: "b"}},
		Combos: []config.ComboConfig{
			{ID: "main", Strategy: "round-robin", Providers: []string{"a", "b"}},
		},
		DefaultCombo: "main",
	}
	built := map[string]bool{"a": true} // "b" failed to build

	got := sanitizeCombosForBuiltProviders(cfg, fakeProviders(built), discardLogger())

	if len(got.Combos) != 1 {
		t.Fatalf("expected the combo to survive, got %d combos", len(got.Combos))
	}
	if want := []string{"a"}; !stringsEqual(got.Combos[0].Providers, want) {
		t.Errorf("combo providers = %v, want %v", got.Combos[0].Providers, want)
	}
	if got.DefaultCombo != "main" {
		t.Errorf("default_combo changed unexpectedly: %q", got.DefaultCombo)
	}
}

// TestSanitizeCombosForBuiltProviders_DropsEmptyComboAndReassignsDefault
// covers the case that used to crash the whole gateway: the combo that
// held the ONLY provider that failed to build, and which also happened
// to be default_combo, must be dropped without taking any other combo
// down, and default_combo must fail over to a combo that still works.
func TestSanitizeCombosForBuiltProviders_DropsEmptyComboAndReassignsDefault(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "anthropic"}, {ID: "openai"}},
		Combos: []config.ComboConfig{
			{ID: "anthropic-only", Strategy: "round-robin", Providers: []string{"anthropic"}},
			{ID: "everything", Strategy: "round-robin", Providers: []string{"anthropic", "openai"}},
		},
		DefaultCombo: "anthropic-only",
	}
	built := map[string]bool{"openai": true} // "anthropic" failed to build

	got := sanitizeCombosForBuiltProviders(cfg, fakeProviders(built), discardLogger())

	if len(got.Combos) != 1 || got.Combos[0].ID != "everything" {
		t.Fatalf("expected only 'everything' to survive, got %+v", got.Combos)
	}
	if got.DefaultCombo != "everything" {
		t.Errorf("expected default_combo to fail over to 'everything', got %q", got.DefaultCombo)
	}
}

func stringsEqual(a, b []string) bool {
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

// TestRun_SkipsProviderMissingCredentialAndServesTheRest reproduces the
// user-reported crash directly: a config with two providers where one's
// credential is missing must not stop the gateway from serving requests
// through the other one.
func TestRun_SkipsProviderMissingCredentialAndServesTheRest(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: port,
		Providers: []config.ProviderConfig{
			{ID: "broken", Kind: "anthropic", APIKeyEnv: "KRAM_TEST_UNSET_KEY_XYZ"}, // KeyOptional false, env unset -> fails to build
			{ID: "local", Kind: "openai-compat", BaseURL: "http://127.0.0.1:1", KeyOptional: true},
		},
		Combos: []config.ComboConfig{
			{ID: "main", Strategy: "round-robin", Providers: []string{"broken", "local"}},
		},
		DefaultCombo: "main",
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, cfg, discardLogger(), nil) }()

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastErr = nil
				break
			}
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("gateway never became healthy despite one working provider: %v", lastErr)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned an error after clean shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

// TestRun_FailsFastWhenEveryProviderFailsToBuild confirms the one case
// that must still be fatal: nothing at all could be built.
func TestRun_FailsFastWhenEveryProviderFailsToBuild(t *testing.T) {
	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: freePort(t),
		Providers: []config.ProviderConfig{
			{ID: "broken", Kind: "anthropic", APIKeyEnv: "KRAM_TEST_UNSET_KEY_XYZ"},
		},
		Combos:       []config.ComboConfig{{ID: "main", Strategy: "round-robin", Providers: []string{"broken"}}},
		DefaultCombo: "main",
	}

	err := Run(context.Background(), cfg, discardLogger(), nil)
	if err == nil {
		t.Fatal("expected an error when every configured provider fails to build")
	}
}

// fakeProviders builds a map with the given IDs present — the values are
// nil since sanitizeCombosForBuiltProviders only checks key presence.
func fakeProviders(ids map[string]bool) map[string]provider.Provider {
	m := make(map[string]provider.Provider, len(ids))
	for id := range ids {
		m[id] = nil
	}
	return m
}
