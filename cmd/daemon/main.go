// Command daemon runs the Kram daemon standalone: the single, local,
// durable owner of sessions and the agent loop that drives them. For the
// all-in-one experience, see cmd/kram, which runs this same logic
// in-process alongside the gateway and CLI.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/codexmark/kram-gateway/internal/daemon"
)

func main() {
	host := flag.String("host", "127.0.0.1", "listen host")
	port := flag.Int("port", 20130, "listen port")
	dbPath := flag.String("db", "kram-daemon.db", "path to the SQLite database file")
	gatewayURL := flag.String("gateway", "http://127.0.0.1:20128", "base URL of a running kram-gateway")
	model := flag.String("model", "default", "gateway combo/model used for new messages")
	workspace := flag.String("workspace", ".", "project root the agent's tools (read/write/grep/bash) operate within")
	maxTurns := flag.Int("max-turns", 50, "iteration budget per agent run, tool round-trips included")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg := daemon.Config{
		Host: *host, Port: *port, DBPath: *dbPath, GatewayURL: *gatewayURL,
		Model: *model, Workspace: *workspace, MaxTurns: *maxTurns,
	}
	if err := daemon.Run(context.Background(), cfg, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}
