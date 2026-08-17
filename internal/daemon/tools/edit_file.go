package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// editFile does a precise find-and-replace instead of rewriting a whole
// file — cheaper in tokens than write_file for a small change, and safer:
// it refuses an ambiguous edit (old_string matching more than once)
// instead of guessing which occurrence was meant.
type editFile struct {
	workspace string
}

func newEditFile(workspace string) *editFile { return &editFile{workspace: workspace} }

func (t *editFile) Name() string { return "edit_file" }
func (t *editFile) Description() string {
	return "Replace an exact substring in a file with another. Fails if old_string isn't found, or is found more than once and replace_all wasn't set — include enough surrounding context in old_string to make it unique."
}

func (t *editFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to the project root."},
			"old_string": {"type": "string", "description": "The exact text to find. Must match exactly, including whitespace."},
			"new_string": {"type": "string", "description": "The text to replace it with."},
			"replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring exactly one match. Defaults to false."}
		},
		"required": ["path", "old_string", "new_string"]
	}`)
}

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *editFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args editFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.OldString == "" {
		return "error: old_string must not be empty", nil
	}
	if args.OldString == args.NewString {
		return "error: old_string and new_string are identical", nil
	}

	path, err := resolvePath(t.workspace, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("error: reading %s: %v", args.Path, err), nil
	}
	content := string(data)

	count := strings.Count(content, args.OldString)
	switch {
	case count == 0:
		return fmt.Sprintf("error: old_string not found in %s", args.Path), nil
	case count > 1 && !args.ReplaceAll:
		return fmt.Sprintf("error: old_string appears %d times in %s — add more context to make it unique, or pass replace_all:true", count, args.Path), nil
	}

	var updated string
	if args.ReplaceAll {
		updated = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		updated = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Sprintf("error: writing %s: %v", args.Path, err), nil
	}

	return fmt.Sprintf("replaced %d occurrence(s) in %s", count, args.Path), nil
}
