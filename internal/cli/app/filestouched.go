package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// filesTouchedShownLimit caps how many chips the summary row shows
// before folding the rest into a "+N more" suffix — a turn with a dozen
// edits shouldn't grow the row into its own scroll-worthy block.
const filesTouchedShownLimit = 6

// mutationArgsPath extracts the path a mutation tool call actually
// touched from its raw JSON arguments — the same {"path": "..."} shape
// edit_file/write_file/delete_file already declare in their own schemas
// (see internal/daemon/tools), so this parses the real wire arguments
// rather than guessing from free text.
func mutationArgsPath(args string) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return ""
	}
	return a.Path
}

// mutationArgsMovePaths extracts move_file's old_path/new_path pair —
// distinct schema from the single-path mutation tools (see
// internal/daemon/tools/move_file.go).
func mutationArgsMovePaths(args string) (oldPath, newPath string) {
	var a struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return "", ""
	}
	return a.OldPath, a.NewPath
}

// touchedFiles collects the distinct file paths a turn's mutation tool
// calls actually touched — edit_file/write_file/delete_file's single
// path, or move_file's old and new paths both (a rename affects two
// locations: one gained the file, one lost it) — deduplicated,
// preserving first-touched order. Read-only tools (read_file, grep,
// glob, list_dir, ...) never contribute; this is deliberately about
// what changed, not what was merely inspected.
func touchedFiles(activities []daemonclient.ToolActivity) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	for _, act := range activities {
		switch act.Name {
		case "edit_file", "write_file", "delete_file":
			add(mutationArgsPath(act.Args))
		case "move_file":
			oldPath, newPath := mutationArgsMovePaths(act.Args)
			add(oldPath)
			add(newPath)
		}
	}
	return out
}

// renderFilesTouched is the turn-ending "what changed" row: the distinct
// paths from touchedFiles, up to filesTouchedShownLimit, with the rest
// folded into a "+N more" suffix rather than growing the row unbounded.
// Empty input renders nothing — a read-only turn (or one with nothing
// yet to summarize) shouldn't grow an empty label.
func renderFilesTouched(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	shown := paths
	overflow := 0
	if len(paths) > filesTouchedShownLimit {
		shown = paths[:filesTouchedShownLimit]
		overflow = len(paths) - filesTouchedShownLimit
	}
	styled := make([]string, len(shown))
	for i, p := range shown {
		styled[i] = styleBadgeAccent.Render(p)
	}
	row := styleHint.Render(filesTouchedLabel) + strings.Join(styled, styleHint.Render(", "))
	if overflow > 0 {
		row += styleHint.Render(fmt.Sprintf(filesTouchedOverflowFmt, overflow))
	}
	return row
}
