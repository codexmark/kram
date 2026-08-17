package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

const (
	gitTimeout     = 15 * time.Second
	gitMaxOutBytes = 30_000
)

// runGit shells out to the system git binary scoped to the workspace —
// read-only subcommands only (status/diff/log), never anything that
// mutates history or the working tree.
func runGit(ctx context.Context, workspace string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "git", args...)
	cmd.Dir = workspace

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	output := out.String()
	if len(output) > gitMaxOutBytes {
		output = output[:gitMaxOutBytes] + "\n\n[output truncated]"
	}
	if output == "" {
		output = "(no output)"
	}
	return output, err
}

type gitStatus struct {
	workspace string
}

func newGitStatus(workspace string) *gitStatus { return &gitStatus{workspace: workspace} }

func (t *gitStatus) Name() string        { return "git_status" }
func (t *gitStatus) Description() string { return "Show the working tree status (git status --short)." }
func (t *gitStatus) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *gitStatus) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	out, _ := runGit(ctx, t.workspace, "status", "--short")
	return out, nil
}

type gitDiff struct {
	workspace string
}

func newGitDiff(workspace string) *gitDiff { return &gitDiff{workspace: workspace} }

func (t *gitDiff) Name() string { return "git_diff" }
func (t *gitDiff) Description() string {
	return "Show uncommitted changes (git diff). Pass staged:true to show staged changes instead."
}
func (t *gitDiff) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {"staged": {"type": "boolean", "description": "Show staged (git diff --cached) instead of unstaged changes."}}}`)
}

func (t *gitDiff) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Staged bool `json:"staged"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &args)
	}

	if args.Staged {
		out, _ := runGit(ctx, t.workspace, "diff", "--cached")
		return out, nil
	}
	out, _ := runGit(ctx, t.workspace, "diff")
	return out, nil
}
