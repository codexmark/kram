package main

import (
	"fmt"
	"os"

	"github.com/codexmark/kram-gateway/internal/config"
)

// knownProvider is one entry in the auto-detection table: if envVar is
// set, this provider is added to the auto-built combo pinned to
// defaultModel.
type knownProvider struct {
	id             string
	kind           string
	baseURL        string
	envVar         string
	defaultModel   string
	supportsImages bool
	supportsTools  bool
}

const openRouterBaseURL = "https://openrouter.ai/api/v1"

var knownProviders = []knownProvider{
	{id: "anthropic", kind: "anthropic", envVar: "ANTHROPIC_API_KEY", defaultModel: "claude-sonnet-4-5", supportsImages: true, supportsTools: true},
	{id: "openai", kind: "openai-compat", baseURL: "https://api.openai.com/v1", envVar: "OPENAI_API_KEY", defaultModel: "gpt-5", supportsImages: true, supportsTools: true},
	{id: "gemini", kind: "gemini", envVar: "GEMINI_API_KEY", defaultModel: "gemini-2.5-pro", supportsImages: true, supportsTools: true},
	// OpenRouter free-tier models: several entries sharing one key so they
	// form a real fallback chain (a $0 combo) rather than a single pinned
	// model. Free-model slugs rotate on OpenRouter's end — check
	// https://openrouter.ai/models?max_price=0 and override via -config
	// if one of these has been retired. Conservatively marked as not
	// supporting images: capability varies per free model and Kram never
	// assumes.
	{id: "openrouter-deepseek", kind: "openai-compat", baseURL: openRouterBaseURL, envVar: "OPENROUTER_API_KEY", defaultModel: "deepseek/deepseek-chat-v3.1:free", supportsTools: true},
	{id: "openrouter-llama", kind: "openai-compat", baseURL: openRouterBaseURL, envVar: "OPENROUTER_API_KEY", defaultModel: "meta-llama/llama-3.3-70b-instruct:free", supportsTools: true},
	{id: "openrouter-qwen", kind: "openai-compat", baseURL: openRouterBaseURL, envVar: "OPENROUTER_API_KEY", defaultModel: "qwen/qwen-2.5-72b-instruct:free", supportsTools: true},
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
