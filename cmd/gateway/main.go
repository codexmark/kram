// Command gateway runs kram-gateway standalone: an OpenAI-compatible LLM
// gateway with load balancing, circuit-breaker fallback and telemetry
// across multiple upstream providers. For the all-in-one experience, see
// cmd/kram, which runs this same logic in-process alongside the daemon
// and CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/gateway"
)

func main() {
	os.Exit(mainExit(context.Background(), os.Args[1:], os.Stdout))
}

func mainExit(ctx context.Context, args []string, stdout io.Writer) int {
	if err := runMain(ctx, args, stdout); err != nil {
		slog.New(slog.NewTextHandler(stdout, nil)).Error("fatal", "error", err)
		return 1
	}
	return 0
}

func runMain(ctx context.Context, args []string, output io.Writer) error {
	fs := flag.NewFlagSet("kram-gateway", flag.ContinueOnError)
	fs.SetOutput(output)
	configPath := fs.String("config", "config.yaml", "path to gateway config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	return run(ctx, *configPath, slog.New(slog.NewTextHandler(output, nil)))
}

func run(ctx context.Context, configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	return gateway.Run(ctx, cfg, logger, nil)
}
