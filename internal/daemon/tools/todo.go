package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// TodoItem is one entry in the project's task list.
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // "pending", "in_progress", "completed"
}

// todoStore is a small, file-backed list shared by todo_write and
// todo_read. It's project-wide rather than per-session — Kram's tool
// registry is built once per daemon process, not per session (see
// NewRegistry) — which matches how a single project typically has one
// task list regardless of which conversation is looking at it.
type todoStore struct {
	mu   sync.Mutex
	path string
	list []TodoItem
}

func newTodoStore(workspace string) *todoStore {
	s := &todoStore{path: filepath.Join(workspace, ".kram", "todos.json")}
	if data, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(data, &s.list)
	}
	return s
}

func (s *todoStore) write(items []TodoItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = items

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}

func (s *todoStore) read() []TodoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TodoItem, len(s.list))
	copy(out, s.list)
	return out
}

type todoWrite struct {
	store *todoStore
}

func newTodoWrite(store *todoStore) *todoWrite { return &todoWrite{store: store} }

func (t *todoWrite) Name() string { return "todo_write" }
func (t *todoWrite) Description() string {
	return "Replace the project's task list with the given items. Use this to plan multi-step work and track progress — write the full list each time, including items that haven't changed."
}
func (t *todoWrite) ToolMetadata() ToolMetadata {
	return ToolMetadata{Summary: "For multi-step work: write the plan, keep it updated as you go."}
}

func (t *todoWrite) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"todos": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"content": {"type": "string"},
						"status": {"type": "string", "enum": ["pending", "in_progress", "completed"]}
					},
					"required": ["content", "status"]
				}
			}
		},
		"required": ["todos"]
	}`)
}

type todoWriteArgs struct {
	Todos []TodoItem `json:"todos"`
}

func (t *todoWrite) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args todoWriteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	for i, item := range args.Todos {
		if item.Status == "" {
			args.Todos[i].Status = "pending"
		}
	}
	if err := t.store.write(args.Todos); err != nil {
		return fmt.Sprintf("error: saving todos: %v", err), nil
	}
	return fmt.Sprintf("saved %d todo(s)", len(args.Todos)), nil
}

type todoRead struct {
	store *todoStore
}

func newTodoRead(store *todoStore) *todoRead { return &todoRead{store: store} }

func (t *todoRead) Name() string        { return "todo_read" }
func (t *todoRead) Description() string { return "Read the project's current task list." }
func (t *todoRead) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *todoRead) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	items := t.store.read()
	if len(items) == 0 {
		return "(no todos yet)", nil
	}
	var b strings.Builder
	for _, item := range items {
		mark := " "
		switch item.Status {
		case "in_progress":
			mark = "~"
		case "completed":
			mark = "x"
		}
		fmt.Fprintf(&b, "[%s] %s\n", mark, item.Content)
	}
	return b.String(), nil
}
