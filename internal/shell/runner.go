// Package shell centralizes two things every direct `exec.Command("sh",
// "-c", ...)` call in this codebase used to assume, silently, and got
// wrong on at least one platform: that a POSIX shell exists to run the
// string through, and that killing "the process" is enough to stop
// whatever it started.
//
// Neither assumption holds on Windows: there's no `sh` unless the user
// happens to have Git Bash or WSL installed, and `Process.Kill()` only
// ever touches the one PID it holds, not any child the shell forked
// (`npm start` forking `node`, `sleep 100 &` backgrounded from a
// script). This package picks the right shell for the platform
// (runner_unix.go / runner_windows.go) and gives every caller a
// KillTree that actually reaches the whole tree instead of just the
// shell.
//
// Callers must start commands via this package's Start or Run, not the
// *exec.Cmd's own — that's what lets KillTree find the tree afterward
// (a process group on Unix, a Job Object on Windows).
package shell

import (
	"context"
	"os/exec"
)

// Command builds an *exec.Cmd that runs command through the platform's
// shell, rooted at workspace. The returned Cmd already has its Cancel
// hook wired to KillTree, so if ctx carries a deadline (as bash's
// per-call timeout does), expiring it kills the whole process tree, not
// just the shell — start it with this package's Start or Run.
func Command(ctx context.Context, workspace, command string) *exec.Cmd {
	cmd := newShellCmd(ctx, command)
	cmd.Dir = workspace
	cmd.Cancel = func() error { return KillTree(cmd) }
	return cmd
}

// Start starts cmd (built by Command) and registers whatever
// process-tree bookkeeping the platform needs for KillTree to work
// later. Bookkeeping failure doesn't fail Start: the command is already
// running by that point, so refusing to report it as started would be a
// worse lie than falling back to single-process kill later.
func Start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = afterStart(cmd)
	return nil
}

// Wait waits for cmd to finish and releases any platform bookkeeping
// Start registered (a Job Object handle, on Windows). Use this instead
// of cmd.Wait() directly for anything started with Start — otherwise a
// long-running background command that exits on its own leaks that
// handle for the life of the daemon.
func Wait(cmd *exec.Cmd) error {
	err := cmd.Wait()
	cleanup(cmd)
	return err
}

// Run starts cmd, waits for it, and cleans up — the Start+Wait pairing
// bash and custom tools want, in place of (*exec.Cmd).Run, so a
// context timeout kills the whole tree instead of leaving orphaned
// children behind.
func Run(cmd *exec.Cmd) error {
	if err := Start(cmd); err != nil {
		return err
	}
	return Wait(cmd)
}

// KillTree terminates cmd's entire process tree: the shell process this
// package started, plus everything it spawned, direct or not. Safe to
// call at any point — before Start, after the process has already
// exited, or concurrently with Wait's own cleanup.
func KillTree(cmd *exec.Cmd) error {
	return killTree(cmd)
}

// Describe names the shell Command actually runs on this platform, for
// tool descriptions that would otherwise promise Unix semantics ("sh
// -c") that silently don't hold on Windows.
func Describe() string {
	return describe()
}
