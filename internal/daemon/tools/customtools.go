package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/codexmark/kram-gateway/internal/kramhome"
	"github.com/codexmark/kram-gateway/internal/shell"
)

// customToolDefaultTimeout matches bash's default — a custom tool is
// running arbitrary user-authored commands with the same trust boundary,
// so it gets the same default guard against hanging the loop.
const customToolDefaultTimeout = 30 * time.Second

// toolManifest is a user-authored tool: a JSON Schema and a shell
// command, no Go code involved. This is Kram's v0 plugin system —
// deliberately not a Go plugin (.so files are fragile and break the
// static-binary goal) and deliberately not an embedded scripting
// language (one more dependency, one more portability surface) — just a
// manifest and a process, the same trust boundary bash already has. The
// tool call's raw JSON arguments are piped to the command's stdin;
// whatever it writes to stdout becomes the tool's result.
type toolManifest struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Command        string          `json:"command"`
	Schema         json.RawMessage `json:"schema"`
	TimeoutSeconds int             `json:"timeout_seconds"`
}

type customTool struct {
	workspace string
	manifest  toolManifest
	source    string // manifest file path, for error messages
}

func (t *customTool) Name() string { return t.manifest.Name }
func (t *customTool) Description() string {
	if t.manifest.Description != "" {
		return t.manifest.Description
	}
	return fmt.Sprintf("Custom tool defined in %s.", t.source)
}
func (t *customTool) Schema() json.RawMessage {
	if len(t.manifest.Schema) == 0 {
		return json.RawMessage(`{"type": "object", "properties": {}}`)
	}
	return t.manifest.Schema
}

func (t *customTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	timeout := customToolDefaultTimeout
	if t.manifest.TimeoutSeconds > 0 {
		timeout = time.Duration(t.manifest.TimeoutSeconds) * time.Second
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := shell.Command(cmdCtx, t.workspace, t.manifest.Command)
	cmd.Stdin = bytes.NewReader(raw)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := shell.Run(cmd)
	result := out.String()

	if cmdCtx.Err() == context.DeadlineExceeded {
		return result + fmt.Sprintf("\n\n[command timed out after %s]", timeout), nil
	}
	if runErr != nil {
		// Same non-editorializing framing as bash — see DECISIONS.md:
		// a non-zero exit isn't necessarily a failure, so "error" is the
		// wrong word to hand a model that will read it as one.
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			return result + fmt.Sprintf("\n\n[exit code %d]", exitErr.ExitCode()), nil
		}
		return result + fmt.Sprintf("\n\n[failed to run: %v]", runErr), nil
	}
	if result == "" {
		return "(no output)", nil
	}
	return result, nil
}

// discoverCustomTools scans both manifest directories — project-local
// (workspace/.kram/tools/*.json) and global (kramhome/tools/*.json) —
// same dual-scope convention as skills. A name declared in both wins
// from the project side, matching MCP's LoadConfig merge rule, so a repo
// can override a user's global tool of the same name.
func discoverCustomTools(workspace string) []*customTool {
	seen := make(map[string]bool)
	var out []*customTool

	add := func(tools []*customTool) {
		for _, t := range tools {
			if seen[t.manifest.Name] {
				continue
			}
			seen[t.manifest.Name] = true
			t.workspace = workspace
			out = append(out, t)
		}
	}

	add(scanToolManifests(filepath.Join(workspace, ".kram", "tools")))
	if globalDir, err := kramhome.Path("tools"); err == nil {
		add(scanToolManifests(globalDir))
	}
	return out
}

func scanToolManifests(dir string) []*customTool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*customTool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m toolManifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue // malformed manifest — skip it rather than fail every tool
		}
		if m.Name == "" || m.Command == "" {
			continue // missing the two required fields — not a usable tool
		}
		out = append(out, &customTool{manifest: m, source: path})
	}
	return out
}
