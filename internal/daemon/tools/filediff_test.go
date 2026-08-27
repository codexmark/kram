package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDiffForToolCallEditFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := diffForToolCall(ws, "edit_file", mustArgs(t, map[string]any{
		"path": "f.txt", "old_string": "beta", "new_string": "BETA",
	}))
	if diff == "" {
		t.Fatal("expected a diff for a valid edit")
	}
	if !strings.Contains(diff, "-beta") || !strings.Contains(diff, "+BETA") {
		t.Errorf("diff missing the change:\n%s", diff)
	}
	if !strings.Contains(diff, "a/f.txt") || !strings.Contains(diff, "b/f.txt") {
		t.Errorf("diff missing git-style labels:\n%s", diff)
	}
}

func TestDiffForToolCallEditFileNonApplicable(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("one\none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// old_string not present → would error on apply → no preview.
	if d := diffForToolCall(ws, "edit_file", mustArgs(t, map[string]any{"path": "f.txt", "old_string": "zzz", "new_string": "x"})); d != "" {
		t.Errorf("old_string not found should give no diff, got:\n%s", d)
	}
	// Ambiguous (2 matches, no replace_all) → would error on apply → no preview.
	if d := diffForToolCall(ws, "edit_file", mustArgs(t, map[string]any{"path": "f.txt", "old_string": "one", "new_string": "x"})); d != "" {
		t.Errorf("ambiguous edit should give no diff, got:\n%s", d)
	}
	// replace_all makes the same edit unambiguous → a diff.
	if d := diffForToolCall(ws, "edit_file", mustArgs(t, map[string]any{"path": "f.txt", "old_string": "one", "new_string": "x", "replace_all": true})); d == "" {
		t.Error("replace_all edit should produce a diff")
	}
	// A missing file (edit_file requires it to exist) → no diff.
	if d := diffForToolCall(ws, "edit_file", mustArgs(t, map[string]any{"path": "nope.txt", "old_string": "a", "new_string": "b"})); d != "" {
		t.Errorf("edit of a missing file should give no diff, got:\n%s", d)
	}
}

func TestDiffForToolCallWriteFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("old content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Overwrite an existing file → diff against its current content.
	diff := diffForToolCall(ws, "write_file", mustArgs(t, map[string]any{"path": "f.txt", "content": "new content\n"}))
	if !strings.Contains(diff, "-old content") || !strings.Contains(diff, "+new content") {
		t.Errorf("overwrite diff missing the change:\n%s", diff)
	}

	// A brand-new file diffs against /dev/null (git's created-file convention).
	newDiff := diffForToolCall(ws, "write_file", mustArgs(t, map[string]any{"path": "brand-new.txt", "content": "hello\n"}))
	if !strings.Contains(newDiff, "/dev/null") || !strings.Contains(newDiff, "+hello") {
		t.Errorf("new-file diff = %q, want /dev/null header and +hello", newDiff)
	}

	// Writing identical content → no change → no diff.
	if d := diffForToolCall(ws, "write_file", mustArgs(t, map[string]any{"path": "f.txt", "content": "old content\n"})); d != "" {
		t.Errorf("identical write should give no diff, got:\n%s", d)
	}
}

func TestDiffForToolCallSkipsNonDiffableAndBinary(t *testing.T) {
	ws := t.TempDir()
	// Non-diffable tools.
	for _, name := range []string{"read_file", "delete_file", "move_file", "bash", "glob"} {
		if d := diffForToolCall(ws, name, mustArgs(t, map[string]any{"path": "f.txt"})); d != "" {
			t.Errorf("%s should not produce a diff, got:\n%s", name, d)
		}
	}
	// Binary file → no text diff.
	if err := os.WriteFile(filepath.Join(ws, "bin"), []byte{0x00, 0x01, 0x02, 0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if d := diffForToolCall(ws, "write_file", mustArgs(t, map[string]any{"path": "bin", "content": "text now"})); d != "" {
		t.Errorf("binary file should be skipped, got:\n%s", d)
	}
	// A path escaping the workspace → no diff (resolvePath rejects it).
	if d := diffForToolCall(ws, "write_file", mustArgs(t, map[string]any{"path": "../escape.txt", "content": "x"})); d != "" {
		t.Errorf("workspace escape should give no diff, got:\n%s", d)
	}
}
