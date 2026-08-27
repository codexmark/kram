package main

import (
	"testing"

	"github.com/codexmark/kram/internal/config"
)

func TestResolveMaxContextTokens(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "a", ContextWindow: 128000},
			{ID: "b", ContextWindow: 32768},
			{ID: "c"}, // unknown window
		},
		Combos: []config.ComboConfig{
			{ID: "default", Providers: []string{"a", "b"}},
			{ID: "cheap", Providers: []string{"c"}}, // no known window
		},
		DefaultCombo: "default",
	}

	// Explicit override wins over everything.
	if got := resolveMaxContextTokens(5000, cfg, "default"); got != 5000 {
		t.Errorf("override: got %d, want 5000", got)
	}
	// No override: min window of the requested combo.
	if got := resolveMaxContextTokens(0, cfg, "default"); got != 32768 {
		t.Errorf("default combo: got %d, want 32768 (min of 128000, 32768)", got)
	}
	// Requested combo has no known window → fall back to default_combo's.
	if got := resolveMaxContextTokens(0, cfg, "cheap"); got != 32768 {
		t.Errorf("cheap combo falls back to default_combo: got %d, want 32768", got)
	}
	// Nothing known anywhere → 0 (agent then uses its built-in default).
	empty := &config.Config{Combos: []config.ComboConfig{{ID: "x", Providers: []string{"none"}}}}
	if got := resolveMaxContextTokens(0, empty, "x"); got != 0 {
		t.Errorf("no known windows: got %d, want 0", got)
	}
}
