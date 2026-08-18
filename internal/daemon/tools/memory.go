package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codexmark/kram-gateway/internal/daemon/store"
)

// maxScopeMemoryChars caps how much memory one scope can hold. Adapted
// from Hermes Agent, whose memory files are hard-capped (~2,200 chars for
// its MEMORY.md, ~1,375 for USER.md) — the cap is the design, not a
// limitation: unbounded memory silently becomes an unbounded prompt
// prefix on every single turn, and "summarize when it gets long" only
// ever happens if something forces it. Hitting the cap returns an error
// telling the model to consolidate before retrying, which is what
// actually keeps entries compiled instead of accumulating near-duplicates.
const maxScopeMemoryChars = 2400

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
	return "Save, replace, or remove a fact in persistent memory that outlives this conversation. Use operation \"add\" (default) for something new, \"replace\" with an id to merge or correct an existing entry, \"remove\" with an id to drop one. Write compiled one-sentence notes, not conversation excerpts. Scope \"project\" is for this codebase, \"global\" for things true everywhere (who the user is, how they like to work). Memory is size-capped per scope: if a write would overflow it, you get the current entries back and must consolidate them with replace/remove before adding more."
}

func (t *memoryWrite) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"operation": {"type": "string", "enum": ["add", "replace", "remove"], "description": "\"add\" (default) stores a new entry, \"replace\" overwrites the entry named by id, \"remove\" deletes it."},
			"id": {"type": "integer", "description": "Entry id, required for replace and remove. Ids come from the injected memory list or from memory_search."},
			"content": {"type": "string", "description": "The compiled fact to remember, written concisely. Required for add and replace."},
			"scope": {"type": "string", "enum": ["project", "global"], "description": "\"project\" (default) for this codebase only, \"global\" for everywhere."},
			"pinned": {"type": "boolean", "description": "Pin so it's always included in the automatic memory injection, not just findable via search. Use sparingly."}
		}
	}`)
}

type memoryWriteArgs struct {
	Operation string `json:"operation"`
	ID        int64  `json:"id"`
	Content   string `json:"content"`
	Scope     string `json:"scope"`
	Pinned    bool   `json:"pinned"`
}

func (t *memoryWrite) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args memoryWriteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}

	scope := t.workspace
	if args.Scope == "global" {
		scope = store.GlobalScope
	}

	switch args.Operation {
	case "remove":
		if args.ID == 0 {
			return "error: remove needs an id", nil
		}
		if err := t.store.DeleteMemoryEntry(args.ID); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		return fmt.Sprintf("removed memory entry #%d", args.ID), nil

	case "replace":
		if args.ID == 0 {
			return "error: replace needs an id", nil
		}
		if args.Content == "" {
			return "error: replace needs content", nil
		}
		if err := t.store.UpdateMemoryEntry(args.ID, args.Content); err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		return fmt.Sprintf("replaced memory entry #%d", args.ID), nil

	default: // "add", or unset
		if args.Content == "" {
			return "error: content must not be empty", nil
		}
		// The cap is checked only on add: replace/remove are how the
		// model gets back under it, so blocking those would deadlock.
		used, err := t.store.ScopeMemorySize(scope)
		if err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		if used+len(args.Content) > maxScopeMemoryChars {
			return t.overflowMessage(scope, used), nil
		}
		entry, err := t.store.WriteMemoryEntry(scope, args.Content, args.Pinned)
		if err != nil {
			return fmt.Sprintf("error: saving memory: %v", err), nil
		}
		return fmt.Sprintf("saved memory entry #%d (scope: %s, %d/%d chars used)",
			entry.ID, scopeLabel(scope), used+len(args.Content), maxScopeMemoryChars), nil
	}
}

// overflowMessage is deliberately actionable rather than a bare error: it
// hands back every current entry with its id, so the model can merge or
// drop entries in the same turn instead of having to go re-read memory
// first just to find out what's in the way.
func (t *memoryWrite) overflowMessage(scope string, used int) string {
	entries, err := t.store.ScopeMemory(scope)
	if err != nil {
		return fmt.Sprintf("error: memory for scope %q is full (%d/%d chars) and listing it failed: %v",
			scopeLabel(scope), used, maxScopeMemoryChars, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "error: memory for scope %q is full (%d/%d chars). Consolidate first — merge related entries with operation \"replace\", drop stale ones with \"remove\" — then add the new fact. Current entries:\n\n",
		scopeLabel(scope), used, maxScopeMemoryChars)
	for _, e := range entries {
		pin := ""
		if e.Pinned {
			pin = " (pinned)"
		}
		fmt.Fprintf(&b, "#%d%s: %s\n", e.ID, pin, e.Content)
	}
	return b.String()
}

func scopeLabel(scope string) string {
	if scope == store.GlobalScope {
		return "global"
	}
	return "project"
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
	return "Full-text search over everything saved to persistent memory (this project and global), for anything not already included in the automatic recent-memory context. Returns entry ids, which memory_write can then replace or remove."
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

	var out strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&out, "#%d [%s] %s\n", e.ID, scopeLabel(e.Scope), e.Content)
	}
	return out.String(), nil
}
