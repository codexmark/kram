//go:build !windows

package shell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true within timeout")
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestCommandRunsAndCapturesOutput(t *testing.T) {
	cmd := Command(context.Background(), t.TempDir(), "echo hello")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := Run(cmd); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "hello" {
		t.Errorf("got %q, want %q", out.String(), "hello")
	}
}

func TestCommandUsesWorkspaceAsDir(t *testing.T) {
	dir := t.TempDir()
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	cmd := Command(context.Background(), dir, "pwd")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := Run(cmd); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(out.String())
	if got != wantDir {
		t.Errorf("pwd = %q, want %q", got, wantDir)
	}
}

// TestKillTreeKillsBackgroundedChild is the shell package's own version
// of the child-process cleanup regression test: KillTree must reach a
// process the shell forked and backgrounded, not just the shell itself.
func TestKillTreeKillsBackgroundedChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := dir + "/child.pid"
	cmd := Command(context.Background(), dir, "sleep 100 & echo $! > "+pidFile+"; wait")
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}

	var pidBytes []byte
	waitUntil(t, 2*time.Second, func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil || len(b) == 0 {
			return false
		}
		pidBytes = b
		return true
	})
	childPid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if !pidAlive(childPid) {
		t.Fatalf("child pid %d should be alive before KillTree", childPid)
	}

	if err := KillTree(cmd); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 6*time.Second, func() bool {
		return !pidAlive(childPid)
	})

	// Wait releases the goroutine started by Start's context watcher;
	// the process is already dead so this just reaps it.
	_ = Wait(cmd)
}

func TestKillTreeEscalatesToSigkillWhenTermIgnored(t *testing.T) {
	dir := t.TempDir()
	// trap SIGTERM and ignore it, so KillTree is forced through its
	// SIGKILL escalation path rather than a clean SIGTERM exit.
	cmd := Command(context.Background(), dir, "trap '' TERM; sleep 100")
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	start := time.Now()
	if err := KillTree(cmd); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// Reap the process before checking liveness: a killed direct child
	// of this test process becomes a zombie until Wait()ed, and
	// kill(pid, 0) reports a zombie as "alive" (its PID slot still
	// exists) — this is the standard Unix gotcha, not a KillTree bug.
	_ = Wait(cmd)

	if pidAlive(pid) {
		t.Fatalf("pid %d still alive after KillTree escalation", pid)
	}
	// Should have waited through (roughly) the grace period before
	// escalating, not returned instantly.
	if elapsed < 1*time.Second {
		t.Errorf("KillTree returned suspiciously fast (%s) for a SIGTERM-ignoring process; escalation may not have run", elapsed)
	}
}

func TestCommandTimeoutKillsWholeTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := dir + "/child.pid"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	cmd := Command(ctx, dir, "sleep 30 & echo $! > "+pidFile+"; wait")
	_ = Run(cmd) // expected to return an error/signal status once killed

	var pidBytes []byte
	waitUntil(t, 2*time.Second, func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil || len(b) == 0 {
			return false
		}
		pidBytes = b
		return true
	})
	childPid, _ := strconv.Atoi(strings.TrimSpace(string(pidBytes)))

	if pidAlive(childPid) {
		t.Errorf("child pid %d should have been killed when the context timed out", childPid)
	}
}

func TestDescribeReportsPosixShell(t *testing.T) {
	if Describe() != "/bin/sh -c" {
		t.Errorf("Describe() = %q, want /bin/sh -c", Describe())
	}
}
