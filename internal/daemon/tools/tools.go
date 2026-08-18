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
	"sort"
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
	delegator Delegator
	disabled  map[string]bool
}

// SetDelegator wires the concrete subagent runner into the registry's
// delegate_task tool, after both the registry and the agent.Service that
// implements Delegator exist — daemon.go calls this once during startup.
// Until it's called, delegate_task exists (the model can see it in tool
// definitions) but reports itself unavailable rather than being hidden,
// which would otherwise require rebuilding the registry after the agent
// service is constructed.
func (r *Registry) SetDelegator(d Delegator) { r.delegator = d }

// NewRegistry builds the default tool set scoped to workspace — every file
// and shell tool refuses to operate outside this directory. st, if
// non-nil, backs the memory_write/memory_search tools (cross-session
// memory, scoped to this workspace plus store.GlobalScope); passing nil
// omits those two tools entirely rather than registering ones that would
// always fail. disabled names every tool (or skill — they share one
// namespace) turned off via the CLI's tools/skills screen; nil or empty
// means everything's on. Taking a plain map here rather than importing
// internal/toolsettings keeps this package from depending on the
// settings-storage format — daemon.go loads the store and passes its
// Disabled() map in.
func NewRegistry(workspace string, st *store.Store, disabled map[string]bool) *Registry {
	if disabled == nil {
		disabled = map[string]bool{}
	}
	r := &Registry{workspace: workspace, byName: make(map[string]Tool), disabled: disabled}
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
		newDelegateTask(r),
		newAskQuestion(),
		newSkillList(r),
		newSkillLoad(r),
		newSkillInstall(),
	}
	if st != nil {
		toolList = append(toolList, newMemoryWrite(st, workspace), newMemorySearch(st, workspace))
	}
	for _, t := range toolList {
		r.byName[t.Name()] = t
	}
	return r
}

// Definitions returns every *enabled* tool's definition in the gateway's
// wire format, ready to attach to a ChatCompletionRequest — a disabled
// tool is invisible to the model entirely, not just refused if called.
func (r *Registry) Definitions() []openai.Tool {
	out := make([]openai.Tool, 0, len(r.byName))
	for _, t := range r.byName {
		if r.disabled[t.Name()] {
			continue
		}
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
// if the name is unknown or disabled — the agent loop feeds this text
// straight back to the model as the tool's result either way, so the
// model can see and recover from a bad or unavailable tool name itself.
// The disabled check here is defense in depth: Definitions() already
// keeps a disabled tool out of what's offered, but a model can still try
// to call a tool name it saw in an earlier turn before it was disabled.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if r.disabled[name] {
		return fmt.Sprintf("error: tool %q is disabled", name), nil
	}
	t, ok := r.byName[name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", name), nil
	}
	return t.Execute(ctx, args)
}

// ToolInfo is one entry in the registry's full listing (AllTools) — every
// tool that exists, regardless of enabled state, for the CLI's
// tools/skills toggle screen.
type ToolInfo struct {
	Name        string
	Description string
	Disabled    bool
}

// AllTools returns every registered tool (including disabled ones), name
// order, for the settings screen — Definitions() deliberately can't be
// reused here since it already filters disabled tools out.
func (r *Registry) AllTools() []ToolInfo {
	out := make([]ToolInfo, 0, len(r.byName))
	for _, t := range r.byName {
		out = append(out, ToolInfo{Name: t.Name(), Description: t.Description(), Disabled: r.disabled[t.Name()]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Skills lists every discovered skill (project + global), disabled state
// included — same purpose as AllTools but for skills, which aren't
// registered as one fixed Tool each the way built-ins are.
func (r *Registry) Skills() []Skill {
	skills := discoverSkills(r.workspace)
	for i := range skills {
		skills[i].Disabled = r.disabled[skills[i].Name]
	}
	return skills
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
