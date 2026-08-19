package provider

import (
	"context"
	"fmt"

	"github.com/codexmark/kram/internal/config"
)

// Build constructs the concrete adapter for a provider config entry.
// resolve is nil for cfg.AuthMode == "" (today's static-env-var
// behavior, unchanged) and must be non-nil for cfg.AuthMode == "oauth" —
// it's called by the adapter on every request to get a currently-valid,
// possibly-just-refreshed credential (see
// internal/credentials.Store.Resolve), since gateway providers are built
// once at startup and held for the gateway's whole lifetime, long past
// when a short-lived OAuth token would expire. Only openai-responses
// (the ChatGPT-login-only Codex backend) ever uses this path — Anthropic
// connected via browser login ends up with a real, permanent API key
// (see internal/oauthflow/anthropic.go), so it always takes the static
// branch below like any pasted key.
func Build(cfg config.ProviderConfig, resolve func(context.Context) (string, error)) (Provider, error) {
	caps := capabilities{images: cfg.SupportsImages, tools: cfg.SupportsTools}

	if cfg.AuthMode == "oauth" {
		if resolve == nil {
			return nil, fmt.Errorf("provider %q: auth_mode oauth requires a credential resolver", cfg.ID)
		}
		switch cfg.Kind {
		case "openai-responses":
			return NewOpenAIResponses(cfg.ID, cfg.BaseURL, resolve, cfg.Model, caps), nil
		default:
			return nil, fmt.Errorf("provider %q: kind %q has no oauth-based adapter", cfg.ID, cfg.Kind)
		}
	}

	apiKey, err := cfg.APIKey()
	if err != nil {
		return nil, err
	}

	switch cfg.Kind {
	case "anthropic":
		return NewAnthropic(cfg.ID, cfg.BaseURL, apiKey, cfg.Model, caps), nil
	case "gemini":
		return NewGemini(cfg.ID, cfg.BaseURL, apiKey, cfg.Model, caps), nil
	case "openai-compat":
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: base_url is required for kind openai-compat", cfg.ID)
		}
		return NewOpenAICompatible(cfg.ID, cfg.BaseURL, apiKey, cfg.Model, caps), nil
	default:
		return nil, fmt.Errorf("provider %q: unknown kind %q", cfg.ID, cfg.Kind)
	}
}
