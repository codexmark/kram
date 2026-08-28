package main

import (
	"testing"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/daemon/agent"
)

func TestResolvePromptProfile(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "anthropic", Model: "claude-opus-5"},
			{ID: "openai", Model: "gpt-5"},
			{ID: "local", Model: "qwen2.5-coder-14b"},
		},
		Combos: []config.ComboConfig{
			{ID: "frontier", Providers: []string{"anthropic", "openai"}},
			{ID: "mixed", Providers: []string{"anthropic", "local"}},
		},
		DefaultCombo: "frontier",
	}
	if got := resolvePromptProfile(cfg, "frontier"); got != agent.ProfileFrontier {
		t.Fatalf("all-frontier combo = %q, want frontier", got)
	}
	if got := resolvePromptProfile(cfg, "mixed"); got != agent.ProfileCompact {
		t.Fatalf("mixed combo = %q, want compact (fallback can route to the small model)", got)
	}
	// An unknown combo falls back to the default combo, mirroring
	// resolveMaxContextTokens.
	if got := resolvePromptProfile(cfg, "missing"); got != agent.ProfileFrontier {
		t.Fatalf("unknown combo = %q, want the default combo's profile", got)
	}
}
