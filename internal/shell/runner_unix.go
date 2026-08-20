//go:build !windows

package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// killTreeGracePeriod is how long KillTree waits after SIGTERM before
// escalating to SIGKILL — long enough for a well-behaved process to
// catch the signal and exit cleanly, short enough that a stuck bash
// call doesn't feel like it hung twice.
const killTreeGracePeriod = 4 * time.Second

func newShellCmd(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, shellExecutable(), "-c", command)
	// Setpgid makes the shell the leader of its own new process group
	// (pgid == its own pid), rather than inheriting the daemon's group.
	// That's what lets killTree below signal the whole group — including
	// children the shell forked, like `npm start`'s node child, or
	// `sleep 100 &` backgrounded from a script — with one call instead of
	// only ever being able to reach the shell itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// shellExecutable avoids assuming an FHS /bin on every Unix-like target.
// Termux exposes sh under its own prefix; ordinary Linux/macOS still resolve
// their normal PATH entry and retain /bin/sh as a conservative last resort.
func shellExecutable() string {
	if path, err := exec.LookPath("sh"); err == nil {
		return path
	}
	if runtime.GOOS == "android" || os.Getenv("TERMUX_VERSION") != "" {
		if prefix := os.Getenv("PREFIX"); prefix != "" {
			candidate := filepath.Join(prefix, "bin", "sh")
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return "/bin/sh"
}

// afterStart: nothing left to do on Unix. Setpgid (set before Start,
// above) already put the process in its own group before it could fork
// anything, so there's no post-start race window the way there is with
// Windows' Job Objects.
func afterStart(cmd *exec.Cmd) error { return nil }

// cleanup: no bookkeeping was registered by afterStart, so nothing to
// release.
func cleanup(cmd *exec.Cmd) {}

// killTree sends SIGTERM to cmd's entire process group (the negative
// PID form of kill(2)), waits up to killTreeGracePeriod for the group to
// actually disappear, and escalates to SIGKILL if it hasn't.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid // group leader: pgid == pid, thanks to Setpgid

	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}

	deadline := time.Now().Add(killTreeGracePeriod)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !groupAlive(pgid) {
		return nil
	}

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// groupAlive checks whether any process in pgid still exists, using
// signal 0 — POSIX-guaranteed to do permission/existence checking only,
// with no signal actually delivered.
func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

func describe() string { return shellExecutable() + " -c" }
