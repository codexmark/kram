// Command gateway runs kram-gateway standalone: an OpenAI-compatible LLM
// gateway with load balancing, circuit-breaker fallback and telemetry
// across multiple upstream providers. For the all-in-one experience, see
// cmd/kram, which runs this same logic in-process alongside the daemon
// and CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/gateway"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to gateway config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(context.Background(), *configPath, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	return gateway.Run(ctx, cfg, logger, nil)
}
