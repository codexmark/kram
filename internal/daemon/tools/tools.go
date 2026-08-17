// Package tools is the daemon's tool registry: the concrete capabilities
// (file I/O, search, shell) the agent loop can call, each scoped to a
// session's workspace directory so a tool call can never read or write
// outside the project it was invoked for.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codexmark/kram-gateway/internal/daemon/store"
	"github.com/codexmark/kram-gateway/internal/openai"
)

// Tool is one callable capability the agent loop can offer the model.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema (as a raw object) describing the tool's
	// arguments, passed straight through to the gateway/provider.
	Schema() json.RawMessage
	// Execute runs the tool and returns its result as text — every tool's
	// output is text, even for structured data (JSON-encoded as a string),
	// since that's what every provider's tool-result message expects.
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry holds every tool available to the agent loop for one workspace.
type Registry struct {
	workspace string
	byName    map[string]Tool
}

// NewRegistry builds the default tool set scoped to workspace — every file
// and shell tool refuses to operate outside this directory. st, if
// non-nil, backs the memory_write/memory_search tools (cross-session
// memory, scoped to this workspace plus store.GlobalScope); passing nil
// omits those two tools entirely rather than registering ones that would
// always fail.
func NewRegistry(workspace string, st *store.Store) *Registry {
	r := &Registry{workspace: workspace, byName: make(map[string]Tool)}
	todos := newTodoStore(workspace)
	toolList := []Tool{
		newReadFile(workspace),
		newWriteFile(workspace),
		newEditFile(workspace),
		newListDir(workspace),
		newGrep(workspace),
		newGlob(workspace),
		newMoveFile(workspace),
		newDeleteFile(workspace),
		newBash(workspace),
		newGitStatus(workspace),
		newGitDiff(workspace),
		newWebFetch(),
		newTodoWrite(todos),
		newTodoRead(todos),
	}
	if st != nil {
		toolList = append(toolList, newMemoryWrite(st, workspace), newMemorySearch(st, workspace))
	}
	for _, t := range toolList {
		r.byName[t.Name()] = t
	}
	return r
}

// Definitions returns every tool's definition in the gateway's wire format,
// ready to attach to a ChatCompletionRequest.
func (r *Registry) Definitions() []openai.Tool {
	out := make([]openai.Tool, 0, len(r.byName))
	for _, t := range r.byName {
		out = append(out, openai.Tool{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema(),
			},
		})
	}
	return out
}

// Execute runs a named tool, or returns an error result (not a Go error)
// if the name is unknown — the agent loop feeds this text straight back to
// the model as the tool's result either way, so the model can see and
// recover from a bad tool name itself.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	t, ok := r.byName[name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", name), nil
	}
	return t.Execute(ctx, args)
}

// resolvePath joins a tool-supplied relative path to the workspace root
// and refuses to leave it — the boundary that keeps every file/shell tool
// from touching anything outside the project it was invoked for.
func resolvePath(workspace, userPath string) (string, error) {
	if userPath == "" {
		userPath = "."
	}
	joined := filepath.Join(workspace, userPath)
	cleaned := filepath.Clean(joined)

	root := filepath.Clean(workspace)
	if cleaned != root && !strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", userPath)
	}
	return cleaned, nil
}
