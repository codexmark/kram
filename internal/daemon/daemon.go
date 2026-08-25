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

	"github.com/codexmark/kram/internal/daemon/agent"
	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/daemon/server"
	"github.com/codexmark/kram/internal/daemon/session"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/daemon/tools"
	"github.com/codexmark/kram/internal/mcp"
	"github.com/codexmark/kram/internal/toolsettings"
)

// Config configures one daemon instance.
type Config struct {
	Host       string
	Port       int
	DBPath     string
	GatewayURL string
	Model      string // gateway combo used for new messages
	Workspace  string // project root the agent's tools operate within
	MaxTurns   int    // model calls per automatic continuation segment
	// PreferStreaming threads through to agent.Config.PreferStreaming —
	// see that field's own doc comment for the real tradeoff it accepts.
	// This struct's own zero value (false) is buffered, matching
	// agent.Config's own field; cmd/kram's -stream flag defaults to true
	// at that call site so the live indicator's reasoning excerpt works
	// out of the box, but can be turned off per deployment. The concrete
	// reason an opt-out turned out to be necessary, not just
	// theoretical: a large local model whose inference server sends
	// nothing at all (not even a reasoning fragment) during prompt
	// prefill trips router.BoundedPeek's fixed idle timeout well before
	// the first token streams, failing every turn outright — buffered
	// mode has no such window and works fine for the exact same setup.
	PreferStreaming bool
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

	// Best-effort: a missing/unreadable settings file just means nothing's
	// disabled, same as a fresh install — never worth failing startup over.
	var disabled map[string]bool
	if ts, err := toolsettings.Load(); err == nil {
		disabled = ts.Disabled()
	}
	toolRegistry := tools.NewRegistry(absWorkspace, st, disabled)
	// run_background processes are daemon-lifetime, not request-lifetime —
	// they must be killed on shutdown or they'd outlive the daemon that
	// started them as untracked orphans.
	defer toolRegistry.StopBackgroundProcesses()
	// Same reasoning for any language server an lsp_* tool call started
	// lazily — see tools.NewRegistry's lsp.NewManager comment. No LSP
	// tool ever being called means no server was ever started, so this
	// is a no-op in the common case.
	defer toolRegistry.StopLSPServers()

	// MCP servers are third-party processes: connecting is I/O that can
	// hang or fail, so it happens after the registry exists and never
	// blocks startup — an unavailable server costs its own tools, nothing
	// more (see mcp.ConnectAll).
	mcpManager := mcp.ConnectAll(ctx, mcp.LoadConfig(absWorkspace), logger)
	defer mcpManager.Close()
	toolRegistry.RegisterMCP(mcpManager)

	agentSvc, err := agent.New(st, gw, toolRegistry, agent.Config{
		Model: cfg.Model, MaxTurns: cfg.MaxTurns, Workspace: absWorkspace,
		// See this file's own Config.PreferStreaming doc comment for why
		// this is caller-controlled rather than hardcoded, and
		// agent.Config.PreferStreaming's for the tradeoff it accepts.
		PreferStreaming: cfg.PreferStreaming,
	})
	if err != nil {
		return fmt.Errorf("building agent service: %w", err)
	}
	// agentSvc implements tools.Delegator (RunTask) for the delegate_task
	// tool — wired after construction since Registry can't import agent
	// (agent already imports tools) without a cycle.
	toolRegistry.SetDelegator(agentSvc)
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
