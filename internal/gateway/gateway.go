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
	"github.com/codexmark/kram/internal/provider"
	"github.com/codexmark/kram/internal/router"
	"github.com/codexmark/kram/internal/server"
	"github.com/codexmark/kram/internal/telemetry"
)

// Run builds the gateway from cfg and serves until ctx is canceled, then
// shuts down gracefully. It blocks until the server has fully stopped.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	providers := make(map[string]provider.Provider, len(cfg.Providers))
	for _, pc := range cfg.Providers {
		p, err := provider.Build(pc)
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
