// Package daemon wires up and runs the Kram daemon's HTTP server —
// extracted out of cmd/daemon so the unified cmd/kram binary can start a
// daemon in-process (a goroutine, not a subprocess) instead of shelling
// out to a separate binary. Subpackages (store, session, agent, tools,
// server, gatewayclient, compaction) hold the actual logic; this is just
// the wiring cmd/daemon and cmd/kram both need.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/codexmark/kram-gateway/internal/daemon/agent"
	"github.com/codexmark/kram-gateway/internal/daemon/gatewayclient"
	"github.com/codexmark/kram-gateway/internal/daemon/server"
	"github.com/codexmark/kram-gateway/internal/daemon/session"
	"github.com/codexmark/kram-gateway/internal/daemon/store"
	"github.com/codexmark/kram-gateway/internal/daemon/tools"
)

// Config configures one daemon instance.
type Config struct {
	Host       string
	Port       int
	DBPath     string
	GatewayURL string
	Model      string // gateway combo used for new messages
	Workspace  string // project root the agent's tools operate within
	MaxTurns   int    // iteration budget per agent run
}

// Run opens the store, builds the agent loop, and serves the daemon's
// HTTP API until ctx is canceled, then shuts down gracefully. It blocks
// until the server has fully stopped.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	absWorkspace, err := filepath.Abs(cfg.Workspace)
	if err != nil {
		return fmt.Errorf("resolving workspace: %w", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	gw := gatewayclient.New(cfg.GatewayURL)
	sessions := session.New(st)
	toolRegistry := tools.NewRegistry(absWorkspace)
	agentSvc := agent.New(st, gw, toolRegistry, agent.Config{Model: cfg.Model, MaxTurns: cfg.MaxTurns})
	srv := server.New(sessions, agentSvc, logger)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}

	go func() {
		<-ctx.Done()
		_ = httpServer.Shutdown(context.Background())
	}()

	logger.Info("kram-daemon listening", "addr", addr, "db", cfg.DBPath, "gateway", cfg.GatewayURL, "workspace", absWorkspace)
	err = httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
