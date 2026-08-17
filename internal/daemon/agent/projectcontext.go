package agent

import (
	"os"
	"path/filepath"
)

// projectContextFiles is checked in order — the first one found wins.
// AGENTS.md is the convention this project itself follows (see
// opencode/Crush's own use of it); CLAUDE.md is accepted too since many
// projects already have one and there's no reason to make users duplicate it.
var projectContextFiles = []string{"AGENTS.md", "CLAUDE.md"}

// maxProjectContextChars caps how much of the file gets sent — a huge
// AGENTS.md would eat the context budget on every single turn, since this
// is re-sent every request rather than compacted away.
const maxProjectContextChars = 20_000

// loadProjectContext reads the workspace's AGENTS.md/CLAUDE.md if present.
// Read fresh on every Run rather than cached — the file is small and
// editing it should take effect on the very next message, not require a
// daemon restart.
func loadProjectContext(workspace string) (content string, found bool) {
	for _, name := range projectContextFiles {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil {
			continue
		}
		text := string(data)
		if len(text) > maxProjectContextChars {
			text = text[:maxProjectContextChars] + "\n\n[truncated]"
		}
		return text, true
	}
	return "", false
}
