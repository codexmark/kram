package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestStopBackgroundProcessesKillsTrackedProcessTree exercises the
// actual daemon-shutdown path (Registry.StopBackgroundProcesses, called
// from daemon.go's shutdown) end to end: a background process started
// through the registry's run_background tool must be fully dead —
// including any child it forked — once shutdown completes, not left
// running as an orphan.
func TestStopBackgroundProcessesKillsTrackedProcessTree(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	r := NewRegistry(workspace, nil, nil)

	childPidFile := workspace + "/child.pid"
	command := "sleep 100 & echo $! > " + childPidFile + "; wait"
	out, err := r.Execute(context.Background(), "run_background", mustJSON(t, runBackgroundArgs{Command: command}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("expected a start confirmation, got %q", out)
	}

	childPid := readPidFile(t, childPidFile)
	if !pidAlive(childPid) {
		t.Fatalf("child pid %d should be alive before shutdown", childPid)
	}

	r.StopBackgroundProcesses()

	waitUntil(t, 6*time.Second, func() bool {
		return !pidAlive(childPid)
	})
}
