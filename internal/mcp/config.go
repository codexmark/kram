package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

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

// Manager owns every connected server for one daemon.
type Manager struct {
	clients map[string]*Client
	logger  *slog.Logger
}

// ConnectAll dials every enabled server in cfg, in name order so startup
// logs are stable. Failures are logged and skipped rather than fatal: a
// broken or uninstalled MCP server should cost you that server's tools,
// nothing else.
func ConnectAll(ctx context.Context, cfg map[string]ServerConfig, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{clients: make(map[string]*Client), logger: logger}

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
		client, err := Connect(ctx, name, sc)
		if err != nil {
			logger.Warn("mcp server unavailable", "server", name, "error", err)
			continue
		}
		m.clients[name] = client
		logger.Info("mcp server ready", "server", name, "info", client.ServerInfo(), "tools", len(client.Tools()))
	}
	return m
}

// Clients returns every connected server by name.
func (m *Manager) Clients() map[string]*Client {
	if m == nil {
		return nil
	}
	return m.clients
}

// Close shuts every server down.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	for _, c := range m.clients {
		_ = c.Close()
	}
}
