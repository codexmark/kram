package main

import (
	"fmt"
	"os"

	"github.com/codexmark/kram-gateway/internal/config"
)

// knownProvider is one entry in the auto-detection table: if envVar is
// set, this provider is added to the auto-built combo pinned to
// defaultModel. Only providers we've tested adapters against get a
// guessed default model here — OpenRouter/opencode-zen's "right" default
// model is too provider-specific to guess safely, so they're left for
// -config instead.
type knownProvider struct {
	id             string
	kind           string
	baseURL        string
	envVar         string
	defaultModel   string
	supportsImages bool
	supportsTools  bool
}

var knownProviders = []knownProvider{
	{id: "anthropic", kind: "anthropic", envVar: "ANTHROPIC_API_KEY", defaultModel: "claude-sonnet-4-5", supportsImages: true, supportsTools: true},
	{id: "openai", kind: "openai-compat", baseURL: "https://api.openai.com/v1", envVar: "OPENAI_API_KEY", defaultModel: "gpt-5", supportsImages: true, supportsTools: true},
	{id: "gemini", kind: "gemini", envVar: "GEMINI_API_KEY", defaultModel: "gemini-2.5-pro", supportsImages: true, supportsTools: true},
}

// detectGatewayConfig builds a single-combo gateway config from whichever
// knownProviders have their API-key env var set. Order is deterministic
// (the table order above), which also becomes the round-robin combo order.
func detectGatewayConfig() (*config.Config, error) {
	var providers []config.ProviderConfig
	var ids []string

	for _, kp := range knownProviders {
		key := os.Getenv(kp.envVar)
		if key == "" {
			continue
		}
		providers = append(providers, config.ProviderConfig{
			ID: kp.id, Kind: kp.kind, BaseURL: kp.baseURL, APIKeyEnv: kp.envVar,
			Model: kp.defaultModel, SupportsImages: kp.supportsImages, SupportsTools: kp.supportsTools,
		})
		ids = append(ids, kp.id)
	}

	if len(providers) == 0 {
		envVars := make([]string, len(knownProviders))
		for i, kp := range knownProviders {
			envVars[i] = kp.envVar
		}
		return nil, fmt.Errorf("no LLM provider configured: export one of %v, or pass -config with a gateway config.yaml", envVars)
	}

	return &config.Config{
		Providers:    providers,
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "round-robin", Providers: ids}},
		DefaultCombo: "default",
	}, nil
}
