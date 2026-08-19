package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const globMaxResults = 500

type glob struct {
	workspace string
}

func newGlob(workspace string) *glob { return &glob{workspace: workspace} }

func (t *glob) Name() string { return "glob" }
func (t *glob) Description() string {
	return "Find files matching a glob pattern (supports * and ** for any depth of directories), e.g. \"**/*.go\" or \"src/*.ts\"."
}
func (t *glob) ToolMetadata() ToolMetadata {
	return ToolMetadata{Summary: "Find files by name pattern instead of guessing where they live."}
}

func (t *glob) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Glob pattern, e.g. \"**/*.go\"."},
			"path": {"type": "string", "description": "Directory to search under, relative to the project root. Defaults to the project root."}
		},
		"required": ["pattern"]
	}`)
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (t *glob) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args globArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.Pattern == "" {
		return "error: pattern must not be empty", nil
	}

	re, err := regexp.Compile(globToRegex(args.Pattern))
	if err != nil {
		return fmt.Sprintf("error: invalid pattern: %v", err), nil
	}

	root, err := resolvePath(t.workspace, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	var matches []string
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if isIgnoredName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= globMaxResults {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if re.MatchString(rel) {
			fullRel, _ := filepath.Rel(t.workspace, path)
			matches = append(matches, filepath.ToSlash(fullRel))
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return fmt.Sprintf("error: search failed: %v", walkErr), nil
	}

	if len(matches) == 0 {
		return "(no matches)", nil
	}
	sort.Strings(matches)
	out := strings.Join(matches, "\n")
	if len(matches) >= globMaxResults {
		out += fmt.Sprintf("\n\n[truncated at %d matches]", globMaxResults)
	}
	return out, nil
}

// globToRegex converts a shell-glob-like pattern into an anchored regular
// expression. "**/" matches zero or more directories, a bare "**" matches
// anything (including "/"), "*" matches within one path segment, "?"
// matches one character within a segment. This is a practical
// approximation, not full glob-library parity.
func globToRegex(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		default:
			c := pattern[i]
			switch c {
			case '*':
				b.WriteString("[^/]*")
			case '?':
				b.WriteString("[^/]")
			case '.', '+', '^', '$', '(', ')', '[', ']', '{', '}', '|', '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
			i++
		}
	}
	b.WriteString("$")
	return b.String()
}
