package provider

import (
	"fmt"

	"github.com/codexmark/kram-gateway/internal/config"
)

// Build constructs the concrete adapter for a provider config entry.
func Build(cfg config.ProviderConfig) (Provider, error) {
	apiKey, err := cfg.APIKey()
	if err != nil {
		return nil, err
	}

	caps := capabilities{images: cfg.SupportsImages, tools: cfg.SupportsTools}

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
