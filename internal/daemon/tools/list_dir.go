package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type listDir struct {
	workspace string
}

func newListDir(workspace string) *listDir { return &listDir{workspace: workspace} }

func (t *listDir) Name() string { return "list_dir" }
func (t *listDir) Description() string {
	return "List the files and subdirectories directly inside a directory, relative to the project root."
}
func (t *listDir) ToolMetadata() ToolMetadata {
	return ToolMetadata{Summary: "Explore a directory's structure before you change anything in it."}
}

func (t *listDir) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Directory path relative to the project root. Defaults to the project root."}
		}
	}`)
}

type listDirArgs struct {
	Path string `json:"path"`
}

func (t *listDir) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args listDirArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Sprintf("error: invalid arguments: %v", err), nil
		}
	}

	path, err := resolvePath(t.workspace, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Sprintf("error: listing %s: %v", args.Path, err), nil
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var b strings.Builder
	for _, e := range entries {
		if isIgnoredName(e.Name()) {
			continue
		}
		if e.IsDir() {
			b.WriteString(e.Name() + "/\n")
		} else {
			info, err := e.Info()
			if err == nil {
				fmt.Fprintf(&b, "%s (%d bytes)\n", e.Name(), info.Size())
			} else {
				b.WriteString(e.Name() + "\n")
			}
		}
	}
	if b.Len() == 0 {
		return "(empty directory)", nil
	}
	return b.String(), nil
}
