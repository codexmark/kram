package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codexmark/kram/internal/mcp"
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
// registry, plus four generic tools covering resources and prompts
// (mcp_resource_list/read, mcp_prompt_list/get) — those aren't
// per-server-per-item like tools are (a server can expose an unbounded,
// changing set of resources), so they're cross-cutting tools that take a
// server name as an argument rather than one registered tool per item.
// Called after NewRegistry because connecting to servers is I/O that can
// block or fail, and the registry has to exist (and the daemon has to be
// able to start) either way.
func (r *Registry) RegisterMCP(m *mcp.Manager) {
	for serverName, client := range m.Clients() {
		for _, t := range client.Tools() {
			tool := newMCPTool(client, serverName, t)
			r.byName[tool.Name()] = tool
		}
	}
	if len(m.Clients()) == 0 {
		return // no servers connected — no point offering tools that would always fail
	}
	for _, t := range []Tool{
		newMCPResourceList(m), newMCPResourceRead(m),
		newMCPPromptList(m), newMCPPromptGet(m),
	} {
		r.byName[t.Name()] = t
	}
}

// mcpServerArg is embedded by every resources/prompts tool: server is
// optional for the "list" tools (omit to query every connected server)
// and required for anything that reads/resolves one specific item, since
// an item's identity (a URI, a prompt name) is only meaningful within the
// server that defined it.
func resolveMCPServer(m *mcp.Manager, name string) (*mcp.Client, error) {
	c, ok := m.Clients()[name]
	if !ok {
		return nil, fmt.Errorf("no connected MCP server named %q", name)
	}
	return c, nil
}

type mcpResourceList struct{ manager *mcp.Manager }

func newMCPResourceList(m *mcp.Manager) *mcpResourceList { return &mcpResourceList{manager: m} }

func (t *mcpResourceList) Name() string { return "mcp_resource_list" }
func (t *mcpResourceList) Description() string {
	return "List resources (server-exposed readable data, identified by URI) from a connected MCP server. Omit server to list from every connected server at once."
}
func (t *mcpResourceList) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"server": {"type": "string", "description": "Name of one connected MCP server. Omit to query all of them."}
		}
	}`)
}

type mcpServerOnlyArgs struct {
	Server string `json:"server"`
}

func (t *mcpResourceList) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args mcpServerOnlyArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}

	servers := t.manager.Clients()
	if args.Server != "" {
		c, err := resolveMCPServer(t.manager, args.Server)
		if err != nil {
			return "error: " + err.Error(), nil
		}
		servers = map[string]*mcp.Client{args.Server: c}
	}

	var b strings.Builder
	found := false
	for name, c := range servers {
		resources, err := c.ListResources(ctx)
		if err != nil {
			fmt.Fprintf(&b, "[%s] error: %v\n", name, err)
			continue
		}
		for _, r := range resources {
			found = true
			fmt.Fprintf(&b, "[%s] %s — %s (%s)\n", name, r.URI, r.Name, r.Description)
		}
	}
	if !found {
		return "(no resources)", nil
	}
	return b.String(), nil
}

type mcpResourceRead struct{ manager *mcp.Manager }

func newMCPResourceRead(m *mcp.Manager) *mcpResourceRead { return &mcpResourceRead{manager: m} }

func (t *mcpResourceRead) Name() string { return "mcp_resource_read" }
func (t *mcpResourceRead) Description() string {
	return "Read one resource's content by URI from a connected MCP server (see mcp_resource_list)."
}
func (t *mcpResourceRead) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"server": {"type": "string", "description": "Name of the connected MCP server that owns this resource."},
			"uri": {"type": "string", "description": "Resource URI, as returned by mcp_resource_list."}
		},
		"required": ["server", "uri"]
	}`)
}

type mcpResourceReadArgs struct {
	Server string `json:"server"`
	URI    string `json:"uri"`
}

func (t *mcpResourceRead) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args mcpResourceReadArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	c, err := resolveMCPServer(t.manager, args.Server)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	result, err := c.ReadResource(ctx, args.URI)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return result, nil
}

type mcpPromptList struct{ manager *mcp.Manager }

func newMCPPromptList(m *mcp.Manager) *mcpPromptList { return &mcpPromptList{manager: m} }

func (t *mcpPromptList) Name() string { return "mcp_prompt_list" }
func (t *mcpPromptList) Description() string {
	return "List prompt templates from a connected MCP server. Omit server to list from every connected server at once."
}
func (t *mcpPromptList) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"server": {"type": "string", "description": "Name of one connected MCP server. Omit to query all of them."}
		}
	}`)
}
func (t *mcpPromptList) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args mcpServerOnlyArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}

	servers := t.manager.Clients()
	if args.Server != "" {
		c, err := resolveMCPServer(t.manager, args.Server)
		if err != nil {
			return "error: " + err.Error(), nil
		}
		servers = map[string]*mcp.Client{args.Server: c}
	}

	var b strings.Builder
	found := false
	for name, c := range servers {
		prompts, err := c.ListPrompts(ctx)
		if err != nil {
			fmt.Fprintf(&b, "[%s] error: %v\n", name, err)
			continue
		}
		for _, p := range prompts {
			found = true
			fmt.Fprintf(&b, "[%s] %s — %s\n", name, p.Name, p.Description)
		}
	}
	if !found {
		return "(no prompts)", nil
	}
	return b.String(), nil
}

type mcpPromptGet struct{ manager *mcp.Manager }

func newMCPPromptGet(m *mcp.Manager) *mcpPromptGet { return &mcpPromptGet{manager: m} }

func (t *mcpPromptGet) Name() string { return "mcp_prompt_get" }
func (t *mcpPromptGet) Description() string {
	return "Resolve one named prompt template with arguments, from a connected MCP server (see mcp_prompt_list)."
}
func (t *mcpPromptGet) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"server": {"type": "string", "description": "Name of the connected MCP server that owns this prompt."},
			"name": {"type": "string", "description": "Prompt name, as returned by mcp_prompt_list."},
			"arguments": {"type": "object", "description": "String key-value arguments the prompt template expects, if any."}
		},
		"required": ["server", "name"]
	}`)
}

type mcpPromptGetArgs struct {
	Server    string            `json:"server"`
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
}

func (t *mcpPromptGet) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args mcpPromptGetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	c, err := resolveMCPServer(t.manager, args.Server)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	result, err := c.GetPrompt(ctx, args.Name, args.Arguments)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return result, nil
}
