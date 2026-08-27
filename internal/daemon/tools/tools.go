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
	"sync"

	"github.com/codexmark/kram/internal/artifact"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/lsp"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/permission"
	"github.com/codexmark/kram/internal/snapshot"
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
	workspace  string
	byName     map[string]Tool
	delegator  Delegator
	disabledMu sync.RWMutex
	disabled   map[string]bool
	processes  *processManager
	permEval   *permission.Evaluator
	grants     *permission.GrantStore
	lspManager *lsp.Manager
	artifacts  *artifact.Store
}

// StopBackgroundProcesses kills every process started by run_background —
// called on daemon shutdown so a background dev server doesn't outlive
// the daemon that started it as an orphan.
func (r *Registry) StopBackgroundProcesses() {
	if r.processes != nil {
		r.processes.killAll()
	}
}

// BackgroundProcesses exposes read-only process metadata to the daemon's
// local control surface. Mutation remains confined to process_kill and daemon
// shutdown, preserving the permission boundary around destructive actions.
func (r *Registry) BackgroundProcesses() []BackgroundProcessInfo {
	if r.processes == nil {
		return nil
	}
	return r.processes.infos()
}

// BackgroundProcessOutput returns an incremental output snapshot for id.
func (r *Registry) BackgroundProcessOutput(id string, cursor *int64) (BackgroundProcessOutput, bool) {
	if r.processes == nil {
		return BackgroundProcessOutput{}, false
	}
	return r.processes.output(id, cursor)
}

// StopLSPServers shuts down every language server this registry's LSP
// tools started, if any — called on daemon shutdown for the same reason
// as StopBackgroundProcesses: a language server is a subprocess Kram
// started, so it's Kram's job to make sure it doesn't outlive the daemon
// as an orphan. A workspace that never called an lsp_* tool never started
// any server, so this is a no-op in the common case.
func (r *Registry) StopLSPServers() {
	if r.lspManager != nil {
		r.lspManager.Close()
	}
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
// memory, scoped to this workspace plus store.GlobalScope) and
// session_search (deterministic full-text search over real conversation
// history, a distinct concern from curated memory — see
// internal/daemon/store/search.go); passing nil omits those tools
// entirely rather than registering ones that would always fail. disabled
// names every tool (or skill — they share one
// namespace) turned off via the CLI's tools/skills screen; nil or empty
// means everything's on. Taking a plain map here rather than importing
// internal/toolsettings keeps this package from depending on the
// settings-storage format — daemon.go loads the store and passes its
// Disabled() map in.
func NewRegistry(workspace string, st *store.Store, disabled map[string]bool) *Registry {
	if disabled == nil {
		disabled = map[string]bool{}
	}
	processes := newProcessManager(workspace)
	// Best-effort like every other local-state loader here: a missing or
	// malformed permissions.json/permission_grants.json just means nothing
	// is gated beyond the compatibility default (Allow), never a reason to
	// fail startup.
	grants, err := permission.LoadGrants(workspace)
	if err != nil {
		grants = nil
	}
	permEval := permission.NewEvaluator(permission.LoadConfig(workspace), grants.Rules())
	// lsp.NewManager is side-effect free — it starts no process and reads
	// no config file until the first lsp_* tool call for a given
	// language, so building it here at registry-construction time (i.e.
	// daemon startup) costs nothing and starts nothing, same invariant
	// MCP servers deliberately don't get (those connect eagerly via
	// mcp.ConnectAll — language servers must not).
	lspManager := lsp.NewManager(workspace)
	// artifact.Open is side-effect free (no directory is created until a
	// tool result actually spills) — GC is a best-effort disk-hygiene
	// pass, not correctness-critical, so it runs once per daemon startup
	// rather than on a timer (see artifact.Store.GC).
	artifacts := artifact.Open(workspace)
	artifacts.GC(artifactMaxAge)
	// snapshot.NewStore is side-effect free — the isolated .kram/snapshots
	// git repository isn't created on disk until snapshot_create actually
	// runs, same pattern as artifact.Open above.
	snapshots := snapshot.NewStore(workspace)
	r := &Registry{
		workspace: workspace, byName: make(map[string]Tool), disabled: disabled,
		processes: processes, permEval: permEval, grants: grants, lspManager: lspManager,
		artifacts: artifacts,
	}
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
		newBash(workspace, artifacts),
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
		newArtifactRead(artifacts),
		newRunBackground(processes),
		newProcessList(processes),
		newProcessOutput(processes),
		newProcessKill(processes),
		newLSPDiagnostics(workspace, lspManager),
		newLSPDefinition(workspace, lspManager),
		newLSPReferences(workspace, lspManager),
		newSnapshotCreate(snapshots),
		newSnapshotList(snapshots),
		newSnapshotDiff(snapshots),
		newSnapshotRestore(snapshots),
	}
	if st != nil {
		toolList = append(toolList, newMemoryWrite(st, workspace), newMemorySearch(st, workspace), newSessionSearch(st))
	}
	for _, t := range toolList {
		r.byName[t.Name()] = t
	}
	// Custom (manifest-defined) tools register last and never override a
	// built-in — a user-authored tool named "bash" or "grep" would
	// otherwise silently change what those names do for every other
	// project's worth of muscle memory.
	for _, ct := range discoverCustomTools(workspace, artifacts) {
		if _, exists := r.byName[ct.Name()]; exists {
			continue
		}
		r.byName[ct.Name()] = ct
	}
	return r
}

// VisibleTools returns every tool the model can actually be offered right
// now, name order — the single source of truth Definitions() (the wire
// schema) and internal/daemon/agent's compileToolsOverview (the prompt's
// generated Tools section) both derive from, so a tool can never be
// announced in the prompt without also being callable, or the reverse.
// Before this existed, compileToolsOverview built its list from AllTools
// instead, which only excludes disabled tools — a tool the permission
// policy denies unconditionally (e.g. a Strict preset's "delete_file:
// deny *") stayed disabled=false and so got announced in the prompt with
// no matching function in the wire schema. Filters the same two things
// Definitions() always has: a disabled tool, and one the permission
// policy denies unconditionally (see permission.Evaluator.FullyDenied) —
// a capability the model can never successfully use shouldn't cost
// tokens in the schema or occupy a line in the overview. A tool denied
// only for *some* patterns stays visible, since it's still sometimes
// usable and the policy is enforced at call time either way.
func (r *Registry) VisibleTools() []Tool {
	r.disabledMu.RLock()
	defer r.disabledMu.RUnlock()
	out := make([]Tool, 0, len(r.byName))
	for _, t := range r.byName {
		if r.disabled[t.Name()] || r.permEval.FullyDenied(t.Name()) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Definitions returns every visible tool's definition (see VisibleTools)
// in the gateway's wire format, ready to attach to a ChatCompletionRequest.
func (r *Registry) Definitions() []openai.Tool {
	visible := r.VisibleTools()
	out := make([]openai.Tool, 0, len(visible))
	for _, t := range visible {
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
// if the name is unknown, disabled, or blocked by permission policy — the
// agent loop feeds this text straight back to the model as the tool's
// result either way, so the model can see and recover from a bad or
// unavailable tool name itself. The disabled check here is defense in
// depth: Definitions() already keeps a disabled tool out of what's
// offered, but a model can still try to call a tool name it saw in an
// earlier turn before it was disabled.
//
// Every tool call — built-in, custom (manifest), and MCP-backed alike —
// funnels through this one method, which is what makes them all subject to
// the same permission policy without each tool implementation needing to
// know policy exists.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (result string, err error) {
	// A panic inside a tool — a malformed MCP response, a nil-deref in a
	// custom tool, whatever — must not unwind past here: it would crash
	// the daemon's agent loop mid-turn and, worse, leave the just-persisted
	// assistant tool-call message with no result (see sanitizeToolHistory's
	// orphaned-call repair for why that's so damaging). Turn a panic into a
	// normal tool-error string so the loop records a result and carries on.
	defer func() {
		if rec := recover(); rec != nil {
			result = fmt.Sprintf("error: tool %q panicked: %v", name, rec)
			err = nil
		}
	}()

	r.disabledMu.RLock()
	disabled := r.disabled[name]
	r.disabledMu.RUnlock()
	if disabled {
		return fmt.Sprintf("error: tool %q is disabled", name), nil
	}
	t, ok := r.byName[name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", name), nil
	}

	subject := policySubject(name, args)
	switch r.permEval.Evaluate(name, subject) {
	case permission.Deny:
		return fmt.Sprintf("error: %q is blocked by permission policy", name), nil
	case permission.Ask:
		approver := approverFromContext(ctx)
		if approver == nil {
			return fmt.Sprintf("error: %q requires approval, but approval isn't available in this context", name), nil
		}
		decision, err := approver.Approve(ctx, name, subject)
		if err != nil {
			return fmt.Sprintf("error: approval failed: %v", err), nil
		}
		switch decision {
		case ApprovalDeny:
			return fmt.Sprintf("denied by user: %q was not approved to run", name), nil
		case ApprovalAlways:
			// Best-effort: a failed write here just means the user gets
			// asked again next time, never a reason to block a call the
			// user just explicitly approved.
			_ = r.grants.Add(name, subject)
		}
		// ApprovalOnce and ApprovalAlways both fall through to execute.
	}

	return t.Execute(ctx, args)
}

// policySubject extracts the text a permission Rule's Pattern is matched
// against for one tool call — bash's command, a file tool's path, the
// move-file tool's "old -> new" pair. Everything else (custom manifest
// tools, MCP tools, delegate_task, and any other tool without a single
// obvious subject field) falls back to the raw argument JSON: enough for a
// tool-wide rule ("mcp__github__*: ask") to work, though per-argument
// pattern matching for those is a known simplification — see DECISIONS.md.
func policySubject(name string, args json.RawMessage) string {
	switch name {
	case "bash":
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(args, &a)
		return a.Command
	case "read_file", "write_file", "edit_file", "delete_file":
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(args, &a)
		return a.Path
	case "move_file":
		var a struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
		}
		_ = json.Unmarshal(args, &a)
		return a.OldPath + " -> " + a.NewPath
	default:
		return string(args)
	}
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
	r.disabledMu.RLock()
	defer r.disabledMu.RUnlock()
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
	r.disabledMu.RLock()
	defer r.disabledMu.RUnlock()
	for i := range skills {
		skills[i].Disabled = r.disabled[skills[i].Name]
	}
	return skills
}

// ReplaceDisabled updates the effective tool/skill profile without restarting
// the daemon. The CLI persists the same desired set first, then sends it here;
// subsequent model calls and executions immediately observe the new profile.
func (r *Registry) ReplaceDisabled(names []string) {
	next := make(map[string]bool, len(names))
	for _, name := range names {
		if name != "" {
			next[name] = true
		}
	}
	r.disabledMu.Lock()
	r.disabled = next
	r.disabledMu.Unlock()
}

// resolvePath joins a tool-supplied relative path to the workspace root
// and refuses to leave it — the boundary that keeps every file/shell tool
// from touching anything outside the project it was invoked for.
func resolvePath(workspace, userPath string) (string, error) {
	if userPath == "" {
		userPath = "."
	}
	candidate := userPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workspace, candidate)
	}
	cleaned := filepath.Clean(candidate)

	root := filepath.Clean(workspace)
	if cleaned != root && !strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", userPath)
	}

	// The lexical check above is necessary but not sufficient:
	// filepath.Clean is purely textual, so a symlink *inside* the
	// workspace pointing out (a cloned repo carrying `link -> ~/.ssh`,
	// say) passes it yet resolves outside. Re-check containment against
	// the real, symlink-resolved path — resolving the deepest existing
	// ancestor when the path itself doesn't exist yet (a write_file
	// creating a new file, whose parent dir is where a malicious symlink
	// would live).
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root // workspace should exist; fall back to the lexical root rather than failing every call
	}
	resolved := resolveExistingAncestor(cleaned)
	if resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace (resolves outside it through a symlink)", userPath)
	}
	return cleaned, nil
}

// resolveExistingAncestor returns path with symlinks resolved on its
// deepest existing prefix and the (not-yet-existing) tail re-appended —
// so a path being newly created still gets its parent directories'
// symlinks resolved, which is exactly where a workspace-escaping symlink
// would sit. If nothing along the path exists (or EvalSymlinks fails for
// another reason) path is returned unchanged; the caller's lexical
// containment check already ran, so this degrades to "no worse than
// before this defense", never to a false escape.
func resolveExistingAncestor(path string) string {
	tail := ""
	for p := path; ; {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, tail)
		}
		parent := filepath.Dir(p)
		if parent == p { // reached the filesystem root without resolving anything
			return path
		}
		tail = filepath.Join(filepath.Base(p), tail)
		p = parent
	}
}
