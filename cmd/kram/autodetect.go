package main

import (
	"fmt"
	"os"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/providercatalog"
)

// loadStoredCredentials exports every key saved via the CLI's accounts
// screen into this process's environment, skipping any env var that's
// already set — a real exported env var always takes priority over a
// stored one, so this only fills gaps. Best-effort: a missing or
// unreadable credentials file just means nothing gets filled in, not a
// startup failure — the shell-env path still works exactly as before this
// existed.
func loadStoredCredentials() {
	store, err := credentials.Load()
	if err != nil {
		return
	}
	for envVar, key := range store.All() {
		if os.Getenv(envVar) == "" && key != "" {
			os.Setenv(envVar, key)
		}
	}
}

// detectGatewayConfig builds a single-combo gateway config from whichever
// providercatalog.Providers have a real credential available: their
// API-key env var set (a real env var, or a key loaded from the local
// credentials store and os.Setenv'd by loadStoredCredentials in main.go
// before this runs), or — for a provider connected via browser login
// (SupportsOAuth) rather than a pasted/exported key — a refreshable OAuth
// token in credStore, in which case AuthMode is set to "oauth" so the
// gateway resolves it live instead of expecting a static env var (see
// internal/gateway.Run). credStore may be nil (no OAuth-based providers
// will be found in that case, same as before this parameter existed).
// Order is deterministic (the catalog's order), which also becomes the
// round-robin combo order.
//
// Every registered internal/customprovider entry is also included,
// unconditionally — unlike a catalog provider, a custom one's mere
// existence in that store *is* the "configured" signal, since its API
// key is optional (most local/LAN servers have no auth) and there's no
// env var whose presence would otherwise say "use this one".
func detectGatewayConfig(strategyOverride string, credStore *credentials.Store) (*config.Config, error) {
	var providers []config.ProviderConfig
	var ids []string

	havePaid := false
	for _, p := range providercatalog.Providers {
		key := os.Getenv(p.EnvVar)
		authMode := ""
		if key == "" {
			if credStore == nil {
				continue
			}
			if _, ok := credStore.GetOAuth(p.EnvVar); !ok {
				continue
			}
			authMode = "oauth"
		}
		providers = append(providers, config.ProviderConfig{
			ID: p.ID, Kind: p.Kind, BaseURL: p.BaseURL, APIKeyEnv: p.EnvVar,
			Model: p.DefaultModel, SupportsImages: p.SupportsImages, SupportsTools: p.SupportsTools,
			AuthMode: authMode,
		})
		ids = append(ids, p.ID)
		if !p.FreeTier {
			havePaid = true
		}
	}

	if customStore, err := customprovider.Load(); err == nil {
		for _, cp := range customStore.All() {
			id := "custom-" + cp.ID
			providers = append(providers, config.ProviderConfig{
				ID: id, Kind: "openai-compat", BaseURL: cp.BaseURL, APIKeyEnv: cp.EnvVar,
				Model: cp.Model, SupportsTools: true, KeyOptional: true,
			})
			ids = append(ids, id)
		}
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("no LLM provider configured: export one of %v, pass -config with a gateway config.yaml, or add a key from the accounts screen (press \"a\" on the session picker)", providercatalog.EnvVars())
	}

	strategy := autoStrategy(havePaid)
	if strategyOverride != "" {
		strategy = strategyOverride
	}

	return &config.Config{
		Providers:    providers,
		Combos:       []config.ComboConfig{{ID: "default", Strategy: strategy, Providers: ids}},
		DefaultCombo: "default",
	}, nil
}

// autoStrategy picks how the auto-built combo routes, and the two cases
// genuinely want opposite things:
//
// With a paid provider present, the catalog order is a *priority* order —
// it leads the chain — and the cheapest thing to do is simply keep using
// it. Leaving the strategy empty means no rotation at all, so the same
// provider serves every call in a turn and its server-side prompt cache
// stays warm across the tool round-trips, where an agent resends a large
// near-identical prefix over and over. Rotating there would re-pay full
// price for the same prefix on every round-trip.
//
// With only free tiers, the providers are interchangeable peers and the
// binding constraint is not cost but rate limits, so round-robin's
// proactive spreading is worth more than the cache — you can't benefit
// from a warm cache on a provider that's answering 429.
func autoStrategy(havePaid bool) string {
	if havePaid {
		return ""
	}
	return "round-robin"
}
