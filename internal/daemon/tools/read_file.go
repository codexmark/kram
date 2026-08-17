package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// maxReadBytes caps how much of a file read_file returns — large files get
// truncated rather than blowing up the conversation's token budget in one
// tool result.
const maxReadBytes = 100_000

type readFile struct {
	workspace string
}

func newReadFile(workspace string) *readFile { return &readFile{workspace: workspace} }

func (t *readFile) Name() string { return "read_file" }
func (t *readFile) Description() string {
	return "Read a UTF-8 text file's contents by path, relative to the project root."
}

func (t *readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to the project root."}
		},
		"required": ["path"]
	}`)
}

type readFileArgs struct {
	Path string `json:"path"`
}

func (t *readFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args readFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}

	path, err := resolvePath(t.workspace, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: reading %s: %v", args.Path, err), nil
	}

	if len(data) > maxReadBytes {
		return fmt.Sprintf("%s\n\n[truncated: file is %d bytes, showing first %d]", string(data[:maxReadBytes]), len(data), maxReadBytes), nil
	}
	return string(data), nil
}
