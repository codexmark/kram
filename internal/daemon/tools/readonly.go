package tools

// readOnlyTools is the explicit allowlist of built-ins that neither
// write, execute, interact, nor mutate any state — safe to run
// concurrently within one tool batch. Deliberate exclusions: bash and
// every writing/moving/deleting tool (obviously), ask_question (waits on
// a human), delegate_task (spawns a whole subagent run), run_background/
// process_kill (process lifecycle), snapshot_create/restore (mutate),
// skill_install and memory/todo writers, lsp_* (the language-server
// manager's concurrency safety is unproven — revisit deliberately, not
// by default), and every mcp__* tool (a remote server's semantics aren't
// something Kram can vouch for).
var readOnlyTools = map[string]bool{
	"read_file": true, "list_dir": true, "glob": true, "grep": true,
	"git_status": true, "git_diff": true,
	"memory_search": true, "session_search": true,
	"skill_list": true, "skill": true,
	"artifact_read": true, "todo_read": true,
	"process_list": true, "process_output": true,
	"snapshot_list": true, "snapshot_diff": true,
	"web_fetch": true,
}

// IsReadOnly reports whether name is on the read-only allowlist — the
// gate internal/daemon/agent uses to run contiguous stretches of one
// tool batch concurrently. Unknown names (MCP tools included) are never
// read-only.
func IsReadOnly(name string) bool { return readOnlyTools[name] }
