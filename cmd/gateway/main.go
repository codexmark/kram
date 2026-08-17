// Command gateway runs kram-gateway: an OpenAI-compatible LLM gateway with
// load balancing, circuit-breaker fallback and telemetry across multiple
// upstream providers.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/codexmark/kram-gateway/internal/breaker"
	"github.com/codexmark/kram-gateway/internal/config"
	"github.com/codexmark/kram-gateway/internal/provider"
	"github.com/codexmark/kram-gateway/internal/router"
	"github.com/codexmark/kram-gateway/internal/server"
	"github.com/codexmark/kram-gateway/internal/telemetry"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to gateway config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := run(*configPath, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
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

	rt, err := router.New(cfg, providers, breakers)
	if err != nil {
		return fmt.Errorf("building router: %w", err)
	}

	srv := server.New(cfg, providers, rt, breakers, tel, logger)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	logger.Info("kram-gateway listening", "addr", addr)
	return http.ListenAndServe(addr, srv.Handler())
}
