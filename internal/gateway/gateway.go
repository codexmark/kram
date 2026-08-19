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
		p, err := provider.Build(pc, resolve)
		if err != nil {
			return fmt.Errorf("building provider %q: %w", pc.ID, err)
		}
		providers[pc.ID] = p
		logger.Info("provider ready", "id", pc.ID, "kind", pc.Kind)
	}

	breakers := breaker.NewRegistry()
	tel := telemetry.New()

	rt, err := router.New(cfg, providers, breakers, tel)
	if err != nil {
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
