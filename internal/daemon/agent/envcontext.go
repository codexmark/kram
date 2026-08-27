package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// collectEnvContext builds the dynamic environment part of the preamble
// (#127): today's date, the active combo, and — when the workspace is a
// git repository — branch, a one-line working-tree summary, and the last
// few commits. Frozen once per run (see runLoop), same reasoning as the
// memory snapshot: the preamble is a prompt prefix, and a prefix that
// changes between a run's own tool round-trips throws away the
// provider's cache exactly where the loop makes most of its calls.
//
// Best-effort with a hard rule: on any git failure the affected line is
// *absent*, never guessed — a timed-out `git status` must not present a
// dirty tree as "clean". ctx bounds every git call (each additionally
// capped at 2s with a WaitDelay so a truly hung git can't wedge the run).
func collectEnvContext(ctx context.Context, workspace, model string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Today's date: %s.\n", time.Now().Format("2006-01-02"))
	if model != "" {
		fmt.Fprintf(&b, "Active model combo: %s.\n", model)
	}
	if workspace == "" {
		return b.String() // no workspace, no git probing (git -C "" would probe the daemon's own cwd)
	}

	git := func(args ...string) (string, bool) {
		gctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(gctx, "git", append([]string{"-C", workspace, "--no-optional-locks"}, args...)...)
		// Without WaitDelay, CommandContext kills the process on timeout
		// but Output() can still block on inherited pipes — see
		// exec.Cmd.WaitDelay's doc.
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}

	// --is-inside-work-tree succeeds even on an unborn branch (a fresh
	// `git init` with no commits), where rev-parse HEAD would not.
	if inside, ok := git("rev-parse", "--is-inside-work-tree"); !ok || inside != "true" {
		return b.String()
	}

	// --show-current prints the branch name (unborn included) and empty
	// on a detached HEAD — the discriminator rev-parse --abbrev-ref
	// lacks, which prints the literal "HEAD" when detached.
	head := ""
	if branch, ok := git("branch", "--show-current"); ok && branch != "" {
		head = "branch " + branch
	} else if sha, ok := git("rev-parse", "--short", "HEAD"); ok {
		head = "detached HEAD at " + sha
	}
	if head != "" {
		line := "Git: " + head
		if status, ok := git("status", "--porcelain"); ok {
			state := "working tree clean"
			if status != "" {
				state = fmt.Sprintf("%d file(s) modified or untracked", len(strings.Split(status, "\n")))
			}
			line += ", " + state
		}
		fmt.Fprintf(&b, "%s.\n", line)
	}
	if log, ok := git("log", "--oneline", "-3"); ok && log != "" {
		fmt.Fprintf(&b, "Recent commits:\n%s\n", log)
	}
	return b.String()
}
