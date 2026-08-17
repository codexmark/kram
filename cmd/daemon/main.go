// Command daemon runs the Kram daemon: the single, local, durable owner of
// sessions and their message history. Clients (CLI, HTTP, future TUI) all
// read and write through it, so closing one client never erases work, and
// restarting the daemon itself never loses a session — everything is
// persisted to SQLite before this process reports success to a caller.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/codexmark/kram-gateway/internal/daemon/gatewayclient"
	"github.com/codexmark/kram-gateway/internal/daemon/server"
	"github.com/codexmark/kram-gateway/internal/daemon/session"
	"github.com/codexmark/kram-gateway/internal/daemon/store"
)

func main() {
	host := flag.String("host", "127.0.0.1", "listen host")
	port := flag.Int("port", 20130, "listen port")
	dbPath := flag.String("db", "kram-daemon.db", "path to the SQLite database file")
	gatewayURL := flag.String("gateway", "http://127.0.0.1:20128", "base URL of a running kram-gateway")
	model := flag.String("model", "default", "gateway combo/model used for new messages")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := run(*host, *port, *dbPath, *gatewayURL, *model, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(host string, port int, dbPath, gatewayURL, model string, logger *slog.Logger) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	gw := gatewayclient.New(gatewayURL)
	sessions := session.New(st, gw, model)
	srv := server.New(sessions, logger)

	addr := fmt.Sprintf("%s:%d", host, port)
	logger.Info("kram-daemon listening", "addr", addr, "db", dbPath, "gateway", gatewayURL)
	return http.ListenAndServe(addr, srv.Handler())
}
