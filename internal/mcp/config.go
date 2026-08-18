package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/codexmark/kram-gateway/internal/kramhome"
)

// ServerConfig is one entry in an mcpServers map. The shape deliberately
// matches the de-facto convention Claude Desktop, Claude Code and
// opencode all use (command/args/env for stdio, url/headers for HTTP), so
// an existing mcp.json can be pointed at Kram without rewriting it.
type ServerConfig struct {
	Type    string            `json:"type,omitempty"` // "stdio" (default when command is set) or "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Enabled *bool             `json:"enabled,omitempty"` // nil means enabled
}

type serverKind int

const (
	kindUnknown serverKind = iota
	kindStdio
	kindHTTP
)

func (c ServerConfig) kind() serverKind {
	switch c.Type {
	case "stdio":
		return kindStdio
	case "http", "sse":
		return kindHTTP
	}
	if c.Command != "" {
		return kindStdio
	}
	if c.URL != "" {
		return kindHTTP
	}
	return kindUnknown
}

func (c ServerConfig) enabled() bool { return c.Enabled == nil || *c.Enabled }

type configFile struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// LoadConfig merges the global server list (kramhome/mcp.json) with the
// project's own (<workspace>/.kram/mcp.json). A project entry wins on
// name collision, which is what lets a repo pin a different version of a
// server than the user's global default.
func LoadConfig(workspace string) map[string]ServerConfig {
	out := make(map[string]ServerConfig)
	if global, err := kramhome.Path("mcp.json"); err == nil {
		mergeConfigFile(out, global)
	}
	mergeConfigFile(out, filepath.Join(workspace, ".kram", "mcp.json"))
	return out
}

// mergeConfigFile is best-effort by design: a missing file is the normal
// case (most projects have no MCP servers), and a malformed one must not
// stop the daemon from starting — it just contributes nothing.
func mergeConfigFile(into map[string]ServerConfig, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cf configFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return
	}
	for name, sc := range cf.MCPServers {
		into[name] = sc
	}
}

// Reconnect tuning: exponential backoff (1s, 2s, 4s, 8s, 16s, ...) capped
// at reconnectMaxDelay, giving up entirely after reconnectMaxAttempts —
// deliberately not an infinite retry loop. A server that's genuinely gone
// (uninstalled, misconfigured, the machine it lived on is down) must not
// leave a goroutine spinning on it forever; it costs that server's tools
// until the next daemon restart, the exact same "one broken entry costs
// only itself" contract ConnectAll already follows.
const (
	reconnectBaseDelay   = 1 * time.Second
	reconnectMaxDelay    = 60 * time.Second
	reconnectMaxAttempts = 5
)

// backoffDelay is the wait before reconnect attempt n (1-indexed):
// base*2^(n-1), capped. A pure function so the growth sequence itself is
// unit-testable without sleeping.
func backoffDelay(attempt int) time.Duration {
	d := reconnectBaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= reconnectMaxDelay {
			return reconnectMaxDelay
		}
	}
	return d
}

// Manager owns every connected server for one daemon, and supervises each
// one for the rest of the daemon's life: if a server's transport dies, the
// Manager reconnects it with backoff rather than leaving it dead until the
// next restart.
type Manager struct {
	mu      sync.Mutex
	clients map[string]*Client
	cfgs    map[string]ServerConfig // by server name, for reconnecting
	logger  *slog.Logger

	// ctx/cancel bound every supervisor goroutine's lifetime. Deliberately
	// a Manager-owned child of the ctx passed to ConnectAll, not that ctx
	// directly: Close() needs the authority to stop supervision (and any
	// in-progress backoff sleep) on its own, so a test — or a caller that
	// closes the Manager before its parent ctx is ever canceled — doesn't
	// leak goroutines waiting on a context nobody will cancel.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// dial and sleep are test seams. Production always uses Connect and a
	// real, context-aware sleep; tests substitute a fake dialer (so
	// reconnect logic can be exercised against in-memory transports
	// instead of real processes) and a non-blocking sleep recorder (so
	// backoff growth is verified by the durations requested, not by
	// burning real wall-clock time).
	dial  func(ctx context.Context, name string, cfg ServerConfig) (*Client, error)
	sleep func(ctx context.Context, d time.Duration)
}

func realSleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// newManager builds a Manager with the given dial/sleep implementations —
// split out from ConnectAll so tests can substitute a fake dialer (an
// in-memory transport instead of a real process/socket) and a
// non-blocking sleep recorder (so backoff growth is asserted from the
// durations requested, never by burning real wall-clock time), while
// production always goes through ConnectAll with the real Connect and a
// real, context-aware sleep.
func newManager(ctx context.Context, logger *slog.Logger, dial func(ctx context.Context, name string, cfg ServerConfig) (*Client, error), sleep func(ctx context.Context, d time.Duration)) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	mctx, cancel := context.WithCancel(ctx)
	return &Manager{
		clients: make(map[string]*Client),
		cfgs:    make(map[string]ServerConfig),
		logger:  logger,
		ctx:     mctx,
		cancel:  cancel,
		dial:    dial,
		sleep:   sleep,
	}
}

// ConnectAll dials every enabled server in cfg, in name order so startup
// logs are stable. Failures are logged and skipped rather than fatal: a
// broken or uninstalled MCP server should cost you that server's tools,
// nothing else. Every server that does connect is then supervised for the
// life of ctx — if its transport later dies, the Manager reconnects it
// with backoff (see superviseServer) instead of leaving it dead.
func ConnectAll(ctx context.Context, cfg map[string]ServerConfig, logger *slog.Logger) *Manager {
	m := newManager(ctx, logger, Connect, realSleep)
	m.connectAll(cfg)
	return m
}

// connectAll is ConnectAll's body, on the Manager itself so tests built
// via newManager can drive it with a fake dialer.
func (m *Manager) connectAll(cfg map[string]ServerConfig) {
	names := make([]string, 0, len(cfg))
	for name := range cfg {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		sc := cfg[name]
		if !sc.enabled() {
			continue
		}
		m.cfgs[name] = sc
		client, err := m.dial(m.ctx, name, sc)
		if err != nil {
			m.logger.Warn("mcp server unavailable", "server", name, "error", err)
			continue
		}
		m.mu.Lock()
		m.clients[name] = client
		m.mu.Unlock()
		m.logger.Info("mcp server ready", "server", name, "info", client.ServerInfo(), "tools", len(client.Tools()))
		m.superviseAsync(name)
	}
}

// superviseAsync launches the background goroutine that watches one
// connected server and reconnects it (with backoff) if its transport
// dies. Split out from ConnectAll so a reconnect can re-arm supervision
// for the client it just replaced, and so tests can start supervision for
// a hand-built Manager without going through ConnectAll's real dialing.
func (m *Manager) superviseAsync(name string) {
	m.wg.Add(1)
	go m.superviseServer(name)
}

// superviseServer waits for the named server's current client to die,
// then reconnects it with backoff; on success it loops to supervise the
// new client the same way, and on giving up it returns, leaving the
// server absent from m.clients until the daemon restarts.
func (m *Manager) superviseServer(name string) {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		client, ok := m.clients[name]
		m.mu.Unlock()
		if !ok {
			return // already reconnecting elsewhere, or never connected — nothing to watch
		}

		select {
		case <-client.Done():
		case <-m.ctx.Done():
			return
		}
		if m.ctx.Err() != nil {
			// Manager is shutting down: Close() already canceled ctx before
			// it ever closes a client, so seeing both ready here means this
			// is a deliberate shutdown, not a real disconnect — don't
			// reconnect into a Manager that's going away.
			return
		}

		m.logger.Warn("mcp server disconnected, will attempt to reconnect", "server", name)
		m.mu.Lock()
		delete(m.clients, name) // stop offering its tools while it's down
		m.mu.Unlock()

		if !m.reconnect(name) {
			return
		}
		// Loop back around to supervise the client reconnect() just installed.
	}
}

// reconnect retries dialing name with exponential backoff, up to
// reconnectMaxAttempts. Returns true and installs the new client in
// m.clients on success; returns false (server left absent) once every
// attempt has failed.
func (m *Manager) reconnect(name string) bool {
	cfg := m.cfgs[name]
	for attempt := 1; attempt <= reconnectMaxAttempts; attempt++ {
		m.sleep(m.ctx, backoffDelay(attempt))
		if m.ctx.Err() != nil {
			return false
		}

		client, err := m.dial(m.ctx, name, cfg)
		if err != nil {
			m.logger.Warn("mcp reconnect attempt failed", "server", name, "attempt", attempt, "max_attempts", reconnectMaxAttempts, "error", err)
			continue
		}

		m.mu.Lock()
		m.clients[name] = client
		m.mu.Unlock()
		m.logger.Info("mcp server reconnected", "server", name, "attempt", attempt, "info", client.ServerInfo(), "tools", len(client.Tools()))
		return true
	}
	m.logger.Warn("mcp server unavailable after repeated reconnect attempts, giving up until next restart",
		"server", name, "attempts", reconnectMaxAttempts)
	return false
}

// Clients returns a snapshot of every currently connected server by name.
// A copy, not the live map: reconnect goroutines mutate the Manager's
// internal map concurrently, so handing out the map itself would be a
// data race for any caller that ranges over it (RegisterMCP, the
// resources/prompts tools) while a reconnect is in flight.
func (m *Manager) Clients() map[string]*Client {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*Client, len(m.clients))
	for k, v := range m.clients {
		out[k] = v
	}
	return out
}

// Close stops supervising every server and shuts every currently
// connected one down. Cancels the Manager's own context first — which
// unblocks any supervisor goroutine waiting on a client, and any backoff
// sleep in progress — and waits for all of them to actually exit before
// touching a single client, so a client is never closed out from under a
// supervisor that might otherwise mistake the deliberate shutdown for a
// crash and try to reconnect into a Manager that's going away.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		_ = c.Close()
	}
}
