// Package tools: grep is a pure-Go recursive search over the workspace —
// no dependency on an external ripgrep/grep binary being installed, in
// keeping with Kram's "never crash / always works" goal. It costs some
// performance on very large trees versus a real ripgrep, which is an
// accepted v0 trade-off.
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	grepMaxMatches   = 200
	grepMaxFileBytes = 2_000_000 // skip files larger than this (binary/generated)
)

type grep struct {
	workspace string
}

func newGrep(workspace string) *grep { return &grep{workspace: workspace} }

func (t *grep) Name() string { return "grep" }
func (t *grep) Description() string {
	return "Search text files under a directory for lines matching a regular expression. Returns up to 200 matches as path:line:content."
}

func (t *grep) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "A regular expression (RE2 syntax) to search for."},
			"path": {"type": "string", "description": "Directory to search under, relative to the project root. Defaults to the project root."}
		},
		"required": ["pattern"]
	}`)
}

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (t *grep) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args grepArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.Pattern == "" {
		return "error: pattern must not be empty", nil
	}

	re, err := regexp.Compile(args.Pattern)
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
			return nil // skip unreadable entries rather than aborting the whole search
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
		if len(matches) >= grepMaxMatches {
			return filepath.SkipAll
		}

		info, err := d.Info()
		if err != nil || info.Size() > grepMaxFileBytes {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		rel, _ := filepath.Rel(t.workspace, path)
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if re.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", rel, lineNum, strings.TrimSpace(line)))
				if len(matches) >= grepMaxMatches {
					break
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return fmt.Sprintf("error: search failed: %v", walkErr), nil
	}

	if len(matches) == 0 {
		return "(no matches)", nil
	}
	out := strings.Join(matches, "\n")
	if len(matches) >= grepMaxMatches {
		out += fmt.Sprintf("\n\n[truncated at %d matches]", grepMaxMatches)
	}
	return out, nil
}
