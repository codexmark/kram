package tools

import (
	"encoding/json"
	"os"
	"strings"
	"unicode/utf8"

	udiff "github.com/aymanbagabas/go-udiff"
)

// maxDiffPreviewBytes skips the approval preview for a pathologically large
// file, so we never build a multi-megabyte diff string for a prompt. Normal
// long diffs still render — the TUI windows/scrolls them; this is only the
// safety net for a genuinely huge before/after.
const maxDiffPreviewBytes = 256 * 1024

// diffForToolCall returns a unified diff previewing what a not-yet-applied
// write_file/edit_file call would change, so the approval prompt can show
// the actual change instead of just the path. It returns "" when there's
// nothing safe to show — a non-diffable tool, unparsable args, a path that
// escapes the workspace, an unreadable/binary/oversized file, an edit_file
// whose old_string wouldn't apply cleanly, or no net change — in which case
// the caller falls back to the old tool:path summary.
//
// It reads the current file but never writes: the would-be-new content is
// computed in memory, mirroring edit_file/write_file's own apply logic
// exactly, so the preview matches what the subsequent Execute will do.
func diffForToolCall(workspace, name string, args json.RawMessage) string {
	switch name {
	case "write_file":
		var a writeFileArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return ""
		}
		path, err := resolvePath(workspace, a.Path)
		if err != nil {
			return ""
		}
		old, isNew, ok := readForDiff(path)
		if !ok {
			return ""
		}
		return renderUnified(a.Path, old, a.Content, isNew)
	case "edit_file":
		var a editFileArgs
		if err := json.Unmarshal(args, &a); err != nil || a.OldString == "" {
			return ""
		}
		path, err := resolvePath(workspace, a.Path)
		if err != nil {
			return ""
		}
		old, isNew, ok := readForDiff(path)
		if !ok || isNew {
			return "" // edit_file requires an existing file; a missing one errors on apply
		}
		count := strings.Count(old, a.OldString)
		// Mirror edit_file.Execute exactly: these cases error on apply, so
		// there's nothing meaningful to preview.
		if count == 0 || (count > 1 && !a.ReplaceAll) {
			return ""
		}
		var updated string
		if a.ReplaceAll {
			updated = strings.ReplaceAll(old, a.OldString, a.NewString)
		} else {
			updated = strings.Replace(old, a.OldString, a.NewString, 1)
		}
		return renderUnified(a.Path, old, updated, false)
	default:
		return "" // move_file/delete_file/read_file/bash/... aren't diffable here
	}
}

// readForDiff reads path's current content for a diff. ok is false only for
// a real read error other than "doesn't exist" (a permission problem or a
// directory) — a missing file is the normal write_file-creates-a-new-file
// case, reported via isNew with old == "".
func readForDiff(path string) (old string, isNew, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", true, true
		}
		return "", false, false
	}
	return string(data), false, true
}

// renderUnified produces the git-style unified diff for old -> new, or ""
// when there's nothing worth showing (identical content, too large, or
// binary). A created file diffs against /dev/null, matching git's own
// convention so one TUI colorizer handles both this and snapshot diffs.
func renderUnified(relPath, old, updated string, isNew bool) string {
	if len(old) > maxDiffPreviewBytes || len(updated) > maxDiffPreviewBytes {
		return ""
	}
	if !utf8.ValidString(old) || !utf8.ValidString(updated) {
		return "" // binary — a unified text diff would be meaningless
	}
	oldLabel := "a/" + relPath
	if isNew {
		oldLabel = "/dev/null"
	}
	return udiff.Unified(oldLabel, "b/"+relPath, old, updated) // "" when identical
}
