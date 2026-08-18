package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codexmark/kram/internal/artifact"
)

// artifactRead reads back a slice of a tool result that was too large to
// inline (see spill.go) — the only way to reach one of those results
// again, deliberately not a general file reader: an artifact id only ever
// resolves an object Kram itself created (see artifact.Store.paths),
// never an arbitrary filesystem path.
type artifactReadTool struct {
	store *artifact.Store
}

func newArtifactRead(store *artifact.Store) *artifactReadTool {
	return &artifactReadTool{store: store}
}

func (t *artifactReadTool) Name() string { return "artifact_read" }
func (t *artifactReadTool) Description() string {
	return "Read back a slice of a tool result that was too large to inline and was saved as an artifact (you'll see an \"artifact:<id>\" reference in a tool result when this happens). offset/limit page through it; omit them to read from the start."
}
func (t *artifactReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "The artifact id, exactly as it appeared in the tool result that referenced it."},
			"offset": {"type": "integer", "description": "Byte offset to start reading from (default 0)."},
			"limit": {"type": "integer", "description": "Maximum bytes to read (default and max ~8000)."}
		},
		"required": ["id"]
	}`)
}

type artifactReadArgs struct {
	ID     string `json:"id"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (t *artifactReadTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args artifactReadArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.ID == "" {
		return "error: id must not be empty", nil
	}
	if args.Offset < 0 {
		args.Offset = 0
	}

	text, a, err := t.store.Read(args.ID, args.Offset, args.Limit)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	end := args.Offset + len(text)
	more := ""
	if int64(end) < a.Size {
		more = fmt.Sprintf(" (%d more bytes available — call again with offset %d)", a.Size-int64(end), end)
	}
	return fmt.Sprintf("[artifact %s, %d/%d bytes]%s\n\n%s", a.ID, end-args.Offset, a.Size, more, text), nil
}
