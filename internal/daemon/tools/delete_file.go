package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// deleteFile is deliberately conservative: it refuses to delete a
// directory (recursive deletion is the single easiest way for an agent
// tool to do real damage) and points the model at bash for that case,
// where the command is at least visible in the tool-call log.
type deleteFile struct {
	workspace string
}

func newDeleteFile(workspace string) *deleteFile { return &deleteFile{workspace: workspace} }

func (t *deleteFile) Name() string { return "delete_file" }
func (t *deleteFile) Description() string {
	return "Delete a single file. Refuses to delete directories — use the bash tool with an explicit command for recursive deletes."
}

func (t *deleteFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to the project root."}
		},
		"required": ["path"]
	}`)
}

type deleteFileArgs struct {
	Path string `json:"path"`
}

func (t *deleteFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args deleteFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}

	path, err := resolvePath(t.workspace, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("error: %s: %v", args.Path, err), nil
	}
	if info.IsDir() {
		return fmt.Sprintf("error: %s is a directory — use the bash tool for recursive deletes", args.Path), nil
	}

	if err := os.Remove(path); err != nil {
		return fmt.Sprintf("error: deleting %s: %v", args.Path, err), nil
	}
	return fmt.Sprintf("deleted %s", args.Path), nil
}
