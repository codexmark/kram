package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codexmark/kram-gateway/internal/daemon/store"
)

// memoryScopes returns the scopes a memory read/write should consider:
// this project's workspace path, plus the cross-project global scope.
func memoryScopes(workspace string) []string {
	return []string{workspace, store.GlobalScope}
}

type memoryWrite struct {
	store     *store.Store
	workspace string
}

func newMemoryWrite(st *store.Store, workspace string) *memoryWrite {
	return &memoryWrite{store: st, workspace: workspace}
}

func (t *memoryWrite) Name() string { return "memory_write" }
func (t *memoryWrite) Description() string {
	return "Save a fact, decision, or preference to persistent memory so it's available in future sessions — not just this conversation. Write compiled, concise notes (a sentence or two), not raw conversation excerpts. Use scope \"project\" for things specific to this codebase, or \"global\" for things true across every project (e.g. a user preference)."
}

func (t *memoryWrite) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"content": {"type": "string", "description": "The compiled fact or note to remember, written concisely."},
			"scope": {"type": "string", "enum": ["project", "global"], "description": "\"project\" (default) for this codebase only, \"global\" for everywhere."},
			"pinned": {"type": "boolean", "description": "Pin this entry so it's always included in the automatic recent-memory injection, not just found via search. Use sparingly, for things that matter every session."}
		},
		"required": ["content"]
	}`)
}

type memoryWriteArgs struct {
	Content string `json:"content"`
	Scope   string `json:"scope"`
	Pinned  bool   `json:"pinned"`
}

func (t *memoryWrite) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args memoryWriteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.Content == "" {
		return "error: content must not be empty", nil
	}

	scope := t.workspace
	if args.Scope == "global" {
		scope = store.GlobalScope
	}

	entry, err := t.store.WriteMemoryEntry(scope, args.Content, args.Pinned)
	if err != nil {
		return fmt.Sprintf("error: saving memory: %v", err), nil
	}
	return fmt.Sprintf("saved memory entry #%d (scope: %s)", entry.ID, args.Scope), nil
}

type memorySearch struct {
	store     *store.Store
	workspace string
}

func newMemorySearch(st *store.Store, workspace string) *memorySearch {
	return &memorySearch{store: st, workspace: workspace}
}

func (t *memorySearch) Name() string { return "memory_search" }
func (t *memorySearch) Description() string {
	return "Full-text search over everything saved to persistent memory (this project and global), for anything not already included in the automatic recent-memory context."
}

func (t *memorySearch) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search terms (SQLite FTS5 syntax — plain words are fine)."}
		},
		"required": ["query"]
	}`)
}

type memorySearchArgs struct {
	Query string `json:"query"`
}

const memorySearchLimit = 10

func (t *memorySearch) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args memorySearchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.Query == "" {
		return "error: query must not be empty", nil
	}

	entries, err := t.store.SearchMemory(memoryScopes(t.workspace), args.Query, memorySearchLimit)
	if err != nil {
		return fmt.Sprintf("error: searching memory: %v", err), nil
	}
	if len(entries) == 0 {
		return "(no matching memory)", nil
	}

	var out string
	for _, e := range entries {
		scopeLabel := "project"
		if e.Scope == store.GlobalScope {
			scopeLabel = "global"
		}
		out += fmt.Sprintf("[%s] %s\n", scopeLabel, e.Content)
	}
	return out, nil
}
