package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codexmark/kram-gateway/internal/mcp"
)

// mcpToolPrefix namespaces every tool that came from an MCP server.
// Without it a server publishing a tool called "read_file" or "bash"
// would shadow Kram's own, silently changing what those names do; with
// it, origin is visible to both the model and the user reading the
// tool-activity log, and the toggle screen can tell them apart.
const mcpToolPrefix = "mcp__"

// mcpTool adapts one MCP server tool to Kram's Tool interface. The
// server's JSON Schema is passed straight through — it's already the
// exact shape a provider's tool definition needs.
type mcpTool struct {
	client     *mcp.Client
	serverName string
	remoteName string
	desc       string
	schema     json.RawMessage
}

func newMCPTool(client *mcp.Client, serverName string, t mcp.Tool) *mcpTool {
	desc := t.Description
	if desc == "" {
		desc = fmt.Sprintf("Tool %q from MCP server %q.", t.Name, serverName)
	}
	schema := t.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type": "object", "properties": {}}`)
	}
	return &mcpTool{
		client:     client,
		serverName: serverName,
		remoteName: t.Name,
		desc:       desc,
		schema:     schema,
	}
}

func (t *mcpTool) Name() string            { return mcpToolPrefix + t.serverName + "__" + t.remoteName }
func (t *mcpTool) Description() string     { return t.desc }
func (t *mcpTool) Schema() json.RawMessage { return t.schema }

func (t *mcpTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	result, err := t.client.CallTool(ctx, t.remoteName, args)
	if err != nil {
		// Returned as text, not a Go error, so the model sees what broke
		// and can adapt — same contract as every built-in tool.
		return fmt.Sprintf("error: mcp server %q: %v", t.serverName, err), nil
	}
	return result, nil
}

// RegisterMCP adds every tool from every connected MCP server to the
// registry. Called after NewRegistry because connecting to servers is
// I/O that can block or fail, and the registry has to exist (and the
// daemon has to be able to start) either way.
func (r *Registry) RegisterMCP(m *mcp.Manager) {
	for serverName, client := range m.Clients() {
		for _, t := range client.Tools() {
			tool := newMCPTool(client, serverName, t)
			r.byName[tool.Name()] = tool
		}
	}
}
