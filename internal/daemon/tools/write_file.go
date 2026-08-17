package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type writeFile struct {
	workspace string
}

func newWriteFile(workspace string) *writeFile { return &writeFile{workspace: workspace} }

func (t *writeFile) Name() string { return "write_file" }
func (t *writeFile) Description() string {
	return "Create or overwrite a UTF-8 text file at the given path, relative to the project root. Creates parent directories as needed."
}

func (t *writeFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to the project root."},
			"content": {"type": "string", "description": "The full file content to write."}
		},
		"required": ["path", "content"]
	}`)
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *writeFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args writeFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}

	path, err := resolvePath(t.workspace, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Sprintf("error: creating directories for %s: %v", args.Path, err), nil
	}
	if err := os.WriteFile(path, []byte(args.Content), 0o644); err != nil {
		return fmt.Sprintf("error: writing %s: %v", args.Path, err), nil
	}

	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), nil
}
