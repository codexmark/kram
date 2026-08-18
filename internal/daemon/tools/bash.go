package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/codexmark/kram-gateway/internal/shell"
)

const (
	bashDefaultTimeout = 30 * time.Second
	bashMaxTimeout     = 120 * time.Second
	bashMaxOutputBytes = 50_000
)

// bash runs a shell command in the workspace directory. It is
// foreground-only and always bounded by a timeout — no background
// processes, matching the constraint other agent-loop implementations
// (Hermes) settled on to keep a single tool call from wedging the loop.
type bash struct {
	workspace string
}

func newBash(workspace string) *bash { return &bash{workspace: workspace} }

func (t *bash) Name() string { return "bash" }
func (t *bash) Description() string {
	// The shell named here is real, not assumed — see internal/shell:
	// it's /bin/sh -c on Unix and cmd.exe /S /C on Windows, never
	// promising Unix-only syntax (pipes, &&, backgrounding with &) on a
	// platform where it wouldn't hold.
	return fmt.Sprintf("Run a shell command (via %s) in the project root and return its combined stdout/stderr. Foreground only, bounded by a timeout — do not use for long-running or background processes.", shell.Describe())
}

func (t *bash) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "The shell command to run."},
			"timeout_seconds": {"type": "integer", "description": "Timeout in seconds (default 30, max 120)."}
		},
		"required": ["command"]
	}`)
}

type bashArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (t *bash) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args bashArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.Command == "" {
		return "error: command must not be empty", nil
	}

	timeout := bashDefaultTimeout
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
		if timeout > bashMaxTimeout {
			timeout = bashMaxTimeout
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := shell.Command(cmdCtx, t.workspace, args.Command)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := shell.Run(cmd)

	output := out.String()
	truncated := false
	if len(output) > bashMaxOutputBytes {
		output = output[:bashMaxOutputBytes]
		truncated = true
	}

	// Deterministic noise filtering, keyed on the command that produced
	// this output (see outputfilter.go). Applied before the byte cap
	// below is reported, so what survives the filter is signal rather
	// than whichever bytes happened to come first.
	result := filterCommandOutput(args.Command, output)
	if truncated {
		result += "\n\n[output truncated]"
	}

	if cmdCtx.Err() == context.DeadlineExceeded {
		return result + fmt.Sprintf("\n\n[command timed out after %s]", timeout), nil
	}
	if runErr != nil {
		// "error" framing here is misleading for anything that uses a
		// non-zero exit as a normal outcome (grep: no matches, diff:
		// files differ, test runners: assertions failed) — a weak model
		// reading "[exit error: ...]" reads it as "something broke" and
		// can give up on the whole turn instead of just reading the
		// output. Report the fact (the exit code) without editorializing
		// about whether it's bad; the output itself says what happened.
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
