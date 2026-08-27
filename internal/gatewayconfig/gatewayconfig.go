// Package gatewayconfig builds and reconciles the gateway's provider
// configuration from Kram's local stores — the catalog of known providers
// (internal/providercatalog), user-registered custom endpoints
// (internal/customprovider), and saved credentials (internal/credentials).
//
// It was extracted out of cmd/kram's package main so this logic — the most
// subtle config code in the project, and the one with a documented
// split-brain bug (see Reconcile) — is exercised by real unit tests instead
// of living in the least-testable place, and so a second entrypoint (e.g.
// cmd/gateway) can reuse the exact same reconciliation rather than
// reinventing it.
package gatewayconfig

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/customprovider"
	"github.com/codexmark/kram/internal/providercatalog"
)

// LoadStoredCredentials exports every key saved via the CLI's accounts
// screen into this process's environment, skipping any env var that's
// already set — a real exported env var always takes priority over a
// stored one, so this only fills gaps. Best-effort: a missing or
// unreadable credentials file just means nothing gets filled in, not a
// startup failure — the shell-env path still works exactly as before this
// existed.
func LoadStoredCredentials() {
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

// Detect builds a single-combo gateway config from whichever
// providercatalog.Providers have a real credential available: their
// API-key env var set (a real env var, or a key loaded from the local
// credentials store and os.Setenv'd by LoadStoredCredentials before this
// runs), or — for a provider connected via browser login (SupportsOAuth)
// rather than a pasted/exported key — a refreshable OAuth token in
// credStore, in which case AuthMode is set to "oauth" so the gateway
// resolves it live instead of expecting a static env var (see
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
func Detect(strategyOverride string, credStore *credentials.Store, logger *slog.Logger) (*config.Config, error) {
	var providers []config.ProviderConfig
	var ids []string

	havePaid := false
	for _, p := range providercatalog.Providers {
		pc, ok := catalogProviderConfig(p, credStore)
		if !ok {
			continue
		}
		providers = append(providers, pc)
		ids = append(ids, pc.ID)
		if !p.FreeTier {
			havePaid = true
		}
	}

	if customStore, err := customprovider.Load(); err == nil {
		for _, cp := range customStore.All() {
			pc, ok := customProviderConfig(cp)
			if !ok {
				if logger != nil {
					logger.Warn("skipping custom provider with no model configured", "id", cp.ID, "name", cp.Name)
				}
				continue
			}
			providers = append(providers, pc)
			ids = append(ids, pc.ID)
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

// catalogProviderConfig builds the config.ProviderConfig for one catalog
// entry if it currently has a real credential available (a real env var,
// or a refreshable OAuth token in credStore) — ok is false if neither is
// present, meaning this provider isn't currently usable and shouldn't be
// added anywhere. Shared between Detect (the fresh-build path) and
// Reconcile (the additive-merge path for an already-loaded config.yaml),
// so both stay in sync automatically.
func catalogProviderConfig(p providercatalog.Provider, credStore *credentials.Store) (config.ProviderConfig, bool) {
	key := os.Getenv(p.EnvVar)
	authMode := ""
	if key == "" {
		if credStore == nil {
			return config.ProviderConfig{}, false
		}
		if _, ok := credStore.GetOAuth(p.EnvVar); !ok {
			return config.ProviderConfig{}, false
		}
		authMode = "oauth"
	}
	return config.ProviderConfig{
		ID: p.ID, Kind: p.Kind, BaseURL: p.BaseURL, APIKeyEnv: p.EnvVar,
		Model: p.DefaultModel, SupportsImages: p.SupportsImages, SupportsTools: p.SupportsTools,
		AuthMode: authMode,
	}, true
}

// customProviderConfig builds the config.ProviderConfig for one
// registered custom provider — ok is false only for a custom provider
// with no model configured. That's no longer possible to create through
// customprovider.Store.Add (it now requires one — see Provider.Model's
// doc comment), but this stays defensive for an entry saved before that
// validation existed: skipping it is far safer than forwarding a
// combo's internal routing ID upstream as a fake model name, which is
// what used to happen silently. Otherwise unconditional, unlike a
// catalog entry: existence in the store *is* the "configured" signal
// (see Detect's doc comment). Shared with Reconcile for the same reason
// catalogProviderConfig is.
func customProviderConfig(cp customprovider.Provider) (config.ProviderConfig, bool) {
	if cp.Model == "" {
		return config.ProviderConfig{}, false
	}
	return config.ProviderConfig{
		ID: "custom-" + cp.ID, Kind: "openai-compat", BaseURL: cp.BaseURL, APIKeyEnv: cp.EnvVar,
		Model: cp.Model, SupportsTools: cp.SupportsToolsOrDefault(), KeyOptional: true,
	}, true
}

// Reconcile additively merges any providercatalog entry now backed by a
// real credential, and any registered internal/customprovider entry, that
// isn't already present in cfg's Providers by ID, into cfg — without
// touching any field on a Provider entry that's already there, and without
// touching Strategy, StrategyOptions, Response, or any combo besides
// appending to DefaultCombo's own Providers list. This is what makes an
// account or custom provider added via the Accounts UI *after* a
// config.yaml already existed actually take effect on the next boot,
// instead of staying invisible until the file is hand-edited or deleted
// — the bug this function exists to fix. Every newly-added provider is
// appended to the *end* of DefaultCombo's list (lowest priority), a
// deliberately conservative choice: it can only ever win once every
// hand-configured provider in that combo is unhealthy, so a hand-tuned
// priority ordering is never silently reshuffled. If cfg has nothing new
// to add, cfg is returned unchanged (no spurious copy, no log lines).
//
// Not called for the pure-autodetect fallback tier (no file was loaded
// at all) — that path already calls Detect fresh on every boot and has
// nothing stale to reconcile against.
func Reconcile(cfg *config.Config, credStore *credentials.Store, logger *slog.Logger) *config.Config {
	existing := make(map[string]bool, len(cfg.Providers))
	for _, pc := range cfg.Providers {
		existing[pc.ID] = true
	}

	var added []config.ProviderConfig
	for _, p := range providercatalog.Providers {
		if existing[p.ID] {
			continue
		}
		if pc, ok := catalogProviderConfig(p, credStore); ok {
			added = append(added, pc)
		}
	}
	if customStore, err := customprovider.Load(); err == nil {
		for _, cp := range customStore.All() {
			id := "custom-" + cp.ID
			if existing[id] {
				continue
			}
			if pc, ok := customProviderConfig(cp); ok {
				added = append(added, pc)
			} else if logger != nil {
				logger.Warn("skipping custom provider with no model configured", "id", cp.ID, "name", cp.Name)
			}
		}
	}
	if len(added) == 0 {
		return cfg
	}

	reconciled := *cfg
	reconciled.Providers = append(append([]config.ProviderConfig{}, cfg.Providers...), added...)
	reconciled.Combos = append([]config.ComboConfig{}, cfg.Combos...)
	for i, combo := range reconciled.Combos {
		if combo.ID != reconciled.DefaultCombo {
			continue
		}
		ids := append([]string{}, combo.Providers...)
		for _, pc := range added {
			ids = append(ids, pc.ID)
		}
		reconciled.Combos[i].Providers = ids
	}

	if logger != nil {
		for _, pc := range added {
			logger.Info("reconciled newly-configured provider into existing config.yaml", "id", pc.ID, "kind", pc.Kind)
		}
	}
	return &reconciled
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
