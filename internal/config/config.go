// Package config loads kram-gateway's YAML configuration: which upstream
// providers exist, how they're grouped into fallback combos, and which
// routing strategy each combo uses.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ProviderConfig describes one upstream LLM backend.
type ProviderConfig struct {
	ID string `yaml:"id"`
	// Kind selects the adapter: "anthropic", "gemini", or "openai-compat".
	Kind string `yaml:"kind"`
	// BaseURL is the upstream API root. Optional for kinds with a
	// well-known default (anthropic, gemini).
	BaseURL string `yaml:"base_url,omitempty"`
	// APIKeyEnv names the environment variable holding the credential.
	APIKeyEnv string `yaml:"api_key_env"`
	// Model is the upstream model ID to request, if it should be pinned
	// regardless of what the client asked for. Empty means passthrough.
	Model string `yaml:"model,omitempty"`
}

// APIKey resolves the provider's credential from its configured env var.
func (p ProviderConfig) APIKey() (string, error) {
	if p.APIKeyEnv == "" {
		return "", nil
	}
	v := os.Getenv(p.APIKeyEnv)
	if v == "" {
		return "", fmt.Errorf("provider %q: env var %s is not set", p.ID, p.APIKeyEnv)
	}
	return v, nil
}

// ComboConfig is a named, ordered fallback chain of providers plus the
// strategy used to pick among the healthy ones.
type ComboConfig struct {
	ID        string   `yaml:"id"`
	Strategy  string   `yaml:"strategy"` // "round-robin" in v0
	Providers []string `yaml:"providers"`
}

// Config is the top-level gateway configuration.
type Config struct {
	Host      string           `yaml:"host"`
	Port      int              `yaml:"port"`
	Providers []ProviderConfig `yaml:"providers"`
	Combos    []ComboConfig    `yaml:"combos"`
	// DefaultCombo is used when a request's "model" doesn't match a combo ID.
	DefaultCombo string `yaml:"default_combo"`
}

// Load reads and validates a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 20128
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider is required")
	}
	ids := make(map[string]bool, len(c.Providers))
	for _, p := range c.Providers {
		if p.ID == "" {
			return fmt.Errorf("config: provider missing id")
		}
		if ids[p.ID] {
			return fmt.Errorf("config: duplicate provider id %q", p.ID)
		}
		ids[p.ID] = true
		switch p.Kind {
		case "anthropic", "gemini", "openai-compat":
		default:
			return fmt.Errorf("config: provider %q has unknown kind %q", p.ID, p.Kind)
		}
	}

	if len(c.Combos) == 0 {
		return fmt.Errorf("config: at least one combo is required")
	}
	comboIDs := make(map[string]bool, len(c.Combos))
	for _, combo := range c.Combos {
		if combo.ID == "" {
			return fmt.Errorf("config: combo missing id")
		}
		if comboIDs[combo.ID] {
			return fmt.Errorf("config: duplicate combo id %q", combo.ID)
		}
		comboIDs[combo.ID] = true
		if len(combo.Providers) == 0 {
			return fmt.Errorf("config: combo %q has no providers", combo.ID)
		}
		for _, pid := range combo.Providers {
			if !ids[pid] {
				return fmt.Errorf("config: combo %q references unknown provider %q", combo.ID, pid)
			}
		}
	}

	if c.DefaultCombo != "" && !comboIDs[c.DefaultCombo] {
		return fmt.Errorf("config: default_combo %q is not a defined combo", c.DefaultCombo)
	}
	return nil
}
