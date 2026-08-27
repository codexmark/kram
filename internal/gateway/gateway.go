// Package gateway wires up and runs kram-gateway's HTTP server — extracted
// out of cmd/gateway so the unified cmd/kram binary can start a gateway
// in-process (a goroutine, not a subprocess) instead of shelling out to a
// separate binary.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/codexmark/kram/internal/breaker"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/oauthflow"
	"github.com/codexmark/kram/internal/provider"
	"github.com/codexmark/kram/internal/router"
	"github.com/codexmark/kram/internal/server"
	"github.com/codexmark/kram/internal/telemetry"
)

// Run builds the gateway from cfg and serves until ctx is canceled, then
// shuts down gracefully. It blocks until the server has fully stopped.
// credStore may be nil (the standalone cmd/gateway dev binary and the
// evals harness never connect an OAuth-based provider) — it's only
// consulted for providers with AuthMode "oauth", to build the resolver
// closure that refreshes and re-reads their credential on every request
// instead of once at this function's own single call, which is long past
// by the time a short-lived OAuth token would expire.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger, credStore *credentials.Store) error {
	if logger == nil {
		logger = slog.Default()
	}

	providers := make(map[string]provider.Provider, len(cfg.Providers))
	for _, pc := range cfg.Providers {
		var resolve func(context.Context) (string, error)
		if pc.AuthMode == "oauth" {
			if credStore == nil {
				return fmt.Errorf("provider %q: auth_mode oauth requires credentials, but none were loaded", pc.ID)
			}
			envVar, refreshAdapter := pc.APIKeyEnv, oauthRefreshAdapter(pc.ID)
			resolve = func(ctx context.Context) (string, error) {
				return credStore.Resolve(ctx, envVar, refreshAdapter)
			}
		}
		p, err := provider.Build(pc, resolve, cfg.Tunables.ResolvedProviderTimeout())
		if err != nil {
			// A single provider failing to build (almost always a missing
			// or revoked credential left behind by a stale config.yaml)
			// must not take the whole gateway down — the other providers
			// are perfectly usable, and this is exactly the situation the
			// router's fallback chain exists to absorb. Skip just this
			// one; combos referencing it get sanitized below, and only a
			// gateway left with zero usable providers is treated as fatal.
			logger.Warn("skipping provider that failed to build", "id", pc.ID, "kind", pc.Kind, "error", err)
			continue
		}
		providers[pc.ID] = p
		logger.Info("provider ready", "id", pc.ID, "kind", pc.Kind)
	}
	if len(providers) == 0 {
		return fmt.Errorf("no provider could be built (%d configured, all failed) — check credentials and provider config", len(cfg.Providers))
	}

	breakers := breaker.NewRegistryWithConfig(breaker.Config{
		FailureThreshold: cfg.Tunables.ResolvedBreakerFailureThreshold(),
		Cooldown:         cfg.Tunables.ResolvedBreakerCooldown(),
	})
	tel := telemetry.New()

	routerCfg := sanitizeCombosForBuiltProviders(cfg, providers, logger)
	rt, err := router.New(routerCfg, providers, breakers, tel)
	if err != nil {
		logger.Error("router failed to build", "error", err)
		return fmt.Errorf("building router: %w", err)
	}

	srv := server.New(cfg, providers, rt, breakers, tel, logger)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}

	go func() {
		<-ctx.Done()
		_ = httpServer.Shutdown(context.Background())
	}()

	logger.Info("kram-gateway listening", "addr", addr)
	err = httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// sanitizeCombosForBuiltProviders returns a shallow copy of cfg whose
// combos only reference providers that actually made it into the built
// map — router.New's own combo-building loop still hard-errors on any
// provider ID it's handed that isn't in that map (correctly so: for a
// config.yaml that was hand-edited or generated with a genuine typo,
// that's a real bug worth surfacing loudly). What's sanitized here is
// the *different, softer* case of a provider ID that was valid at
// config-generation time but failed to build just now (e.g. its
// credential was deleted since) — those get dropped from each combo
// instead of taking the combo, or the whole router, down with them.
func sanitizeCombosForBuiltProviders(cfg *config.Config, providers map[string]provider.Provider, logger *slog.Logger) *config.Config {
	sanitized := *cfg
	sanitized.Combos = make([]config.ComboConfig, 0, len(cfg.Combos))
	for _, cc := range cfg.Combos {
		kept := make([]string, 0, len(cc.Providers))
		for _, pid := range cc.Providers {
			if _, ok := providers[pid]; ok {
				kept = append(kept, pid)
			} else {
				logger.Warn("dropping unbuilt provider from combo", "combo", cc.ID, "provider", pid)
			}
		}
		if len(kept) == 0 {
			logger.Warn("combo has no usable providers left, dropping it", "combo", cc.ID)
			continue
		}
		cc.Providers = kept
		sanitized.Combos = append(sanitized.Combos, cc)
	}

	if sanitized.DefaultCombo != "" {
		stillExists := false
		for _, cc := range sanitized.Combos {
			if cc.ID == sanitized.DefaultCombo {
				stillExists = true
				break
			}
		}
		if !stillExists && len(sanitized.Combos) > 0 {
			logger.Warn("default_combo has no usable providers left, falling back to first remaining combo",
				"default_combo", sanitized.DefaultCombo, "fallback", sanitized.Combos[0].ID)
			sanitized.DefaultCombo = sanitized.Combos[0].ID
		}
	}

	return &sanitized
}

// oauthRefreshAdapter bridges oauthflow's Token-returning refresh
// functions to the credentials.OAuthToken shape Store.Resolve expects.
// Duplicated in internal/cli/app/commands.go rather than shared, since
// sharing it would mean picking one of the two packages as the other's
// dependency for an eight-line function — see that copy's doc comment
// for the layering reasoning.
func oauthRefreshAdapter(providerID string) func(ctx context.Context, refreshToken string) (credentials.OAuthToken, error) {
	f := oauthflow.RefreshFunc(providerID)
	if f == nil {
		return nil
	}
	return func(ctx context.Context, refreshToken string) (credentials.OAuthToken, error) {
		tok, err := f(ctx, refreshToken)
		if err != nil {
			return credentials.OAuthToken{}, err
		}
		return credentials.OAuthToken{Access: tok.Access, Refresh: tok.Refresh, ExpiresAt: tok.ExpiresAt}, nil
	}
}
