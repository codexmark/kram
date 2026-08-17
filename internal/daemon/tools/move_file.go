package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type moveFile struct {
	workspace string
}

func newMoveFile(workspace string) *moveFile { return &moveFile{workspace: workspace} }

func (t *moveFile) Name() string { return "move_file" }
func (t *moveFile) Description() string {
	return "Move or rename a file within the project. Creates the destination's parent directories as needed."
}

func (t *moveFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"old_path": {"type": "string", "description": "Current path, relative to the project root."},
			"new_path": {"type": "string", "description": "Destination path, relative to the project root."}
		},
		"required": ["old_path", "new_path"]
	}`)
}

type moveFileArgs struct {
	OldPath string `json:"old_path"`
	NewPath string `json:"new_path"`
}

func (t *moveFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args moveFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}

	oldPath, err := resolvePath(t.workspace, args.OldPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	newPath, err := resolvePath(t.workspace, args.NewPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Sprintf("error: %s: %v", args.OldPath, err), nil
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return fmt.Sprintf("error: creating directories for %s: %v", args.NewPath, err), nil
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Sprintf("error: moving %s to %s: %v", args.OldPath, args.NewPath, err), nil
	}

	return fmt.Sprintf("moved %s to %s", args.OldPath, args.NewPath), nil
}
