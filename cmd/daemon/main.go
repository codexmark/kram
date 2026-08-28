// Command daemon runs the Kram daemon standalone: the single, local,
// durable owner of sessions and the agent loop that drives them. For the
// all-in-one experience, see cmd/kram, which runs this same logic
// in-process alongside the gateway and CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"

	"github.com/codexmark/kram/internal/daemon"
)

var daemonRun = daemon.Run

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
	fs := flag.NewFlagSet("kram-daemon", flag.ContinueOnError)
	fs.SetOutput(output)
	host := fs.String("host", "127.0.0.1", "listen host")
	port := fs.Int("port", 20130, "listen port")
	dbPath := fs.String("db", "kram-daemon.db", "path to the SQLite database file")
	gatewayURL := fs.String("gateway", "http://127.0.0.1:20128", "base URL of a running kram-gateway")
	model := fs.String("model", "default", "gateway combo/model used for new messages")
	workspace := fs.String("workspace", ".", "project root the agent's tools (read/write/grep/bash) operate within")
	maxTurns := fs.Int("max-turns", 50, "model calls per automatic continuation segment (4 segments maximum)")
	stream := fs.Bool("stream", true, "prefer the streaming gateway path (see daemon.Config.PreferStreaming's doc comment for the tradeoff); disable for a slow local model whose server sends nothing during prompt prefill, which can trip the streaming peek's idle timeout")
	authToken := fs.String("auth-token", "", "bearer token required on every HTTP route except /health; empty generates a random one at boot and writes it to a daemon.token file next to the DB for a client to read")
	promptProfile := fs.String("prompt-profile", "", "base-prompt profile: empty (compact, today's prompt) or \"frontier\" for combos made up exclusively of frontier-class models — cmd/kram derives this automatically from the gateway config")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	logger := slog.New(slog.NewTextHandler(output, nil))
	return daemonRun(ctx, daemonConfig(*host, *port, *dbPath, *gatewayURL, *model, *workspace, *maxTurns, *stream, *authToken, *promptProfile), logger)
}

func daemonConfig(host string, port int, dbPath, gatewayURL, model, workspace string, maxTurns int, stream bool, authToken, promptProfile string) daemon.Config {
	return daemon.Config{
		Host: host, Port: port, DBPath: dbPath, GatewayURL: gatewayURL,
		Model: model, Workspace: workspace, MaxTurns: maxTurns,
		PreferStreaming: stream,
		AuthToken:       authToken,
		PromptProfile:   promptProfile,
	}
}
