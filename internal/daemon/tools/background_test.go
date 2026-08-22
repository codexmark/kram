package tools

import (
	"context"
	"encoding/json"
	"os"
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
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true within timeout")
}

func TestRunBackgroundCapturesOutput(t *testing.T) {
	pm := newProcessManager(t.TempDir())
	tool := newRunBackground(pm)

	args, _ := json.Marshal(runBackgroundArgs{Command: "echo hello-from-bg"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "started bg1") {
		t.Errorf("expected a start confirmation naming the process id, got %q", out)
	}

	outputTool := newProcessOutput(pm)
	waitUntil(t, 2*time.Second, func() bool {
		res, _ := outputTool.Execute(context.Background(), mustJSON(t, processIDArgs{ID: "bg1"}))
		return strings.Contains(res, "hello-from-bg")
	})
}

func TestProcessListShowsRunningAndExited(t *testing.T) {
	pm := newProcessManager(t.TempDir())
	run := newRunBackground(pm)
	list := newProcessList(pm)

	_, _ = run.Execute(context.Background(), mustJSON(t, runBackgroundArgs{Command: "true"}))

	waitUntil(t, 2*time.Second, func() bool {
		out, _ := list.Execute(context.Background(), json.RawMessage(`{}`))
		return strings.Contains(out, "exited 0")
	})
}

func TestProcessKillStopsARunningProcess(t *testing.T) {
	pm := newProcessManager(t.TempDir())
	run := newRunBackground(pm)
	kill := newProcessKill(pm)
	list := newProcessList(pm)

	_, _ = run.Execute(context.Background(), mustJSON(t, runBackgroundArgs{Command: "sleep 30"}))

	waitUntil(t, time.Second, func() bool {
		out, _ := list.Execute(context.Background(), json.RawMessage(`{}`))
		return strings.Contains(out, "running")
	})

	out, err := kill.Execute(context.Background(), mustJSON(t, processIDArgs{ID: "bg1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "killed") {
		t.Errorf("expected a kill confirmation, got %q", out)
	}

	waitUntil(t, time.Second, func() bool {
		out, _ := list.Execute(context.Background(), json.RawMessage(`{}`))
		return !strings.Contains(out, "running")
	})
}

func TestProcessKillUnknownID(t *testing.T) {
	pm := newProcessManager(t.TempDir())
	kill := newProcessKill(pm)
	out, err := kill.Execute(context.Background(), mustJSON(t, processIDArgs{ID: "bg999"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("expected an error message for an unknown id, got %q", out)
	}
}

func TestProcessOutputUnknownID(t *testing.T) {
	pm := newProcessManager(t.TempDir())
	outputTool := newProcessOutput(pm)
	out, err := outputTool.Execute(context.Background(), mustJSON(t, processIDArgs{ID: "bg999"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("expected an error message for an unknown id, got %q", out)
	}
}

// TestProcessKillTerminatesChildProcessToo is the critical regression
// test: process_kill must terminate the whole process tree a background
// shell command started, not just the shell process itself. Before the
// shell runner introduced Setpgid+process-group kill, process_kill only
// ever called cmd.Process.Kill() — which kills the "sh -c ..." process
// but leaves any child it forked (here, a backgrounded `sleep`) running
// as an orphan.
func TestProcessKillTerminatesChildProcessToo(t *testing.T) {
	pm := newProcessManager(t.TempDir())
	run := newRunBackground(pm)
	kill := newProcessKill(pm)

	childPidFile := t.TempDir() + "/child.pid"
	// The parent shell backgrounds a long-lived child and waits on it —
	// exactly the shape of a real "start a dev server" command, whose
	// own runtime (node, python, etc.) is a child the shell forked.
	command := "sleep 100 & echo $! > " + childPidFile + "; wait"
	_, err := run.Execute(context.Background(), mustJSON(t, runBackgroundArgs{Command: command}))
	if err != nil {
		t.Fatal(err)
	}

	childPid := readPidFile(t, childPidFile)
	if !pidAlive(childPid) {
		t.Fatalf("child pid %d should be alive before kill", childPid)
	}

	out, err := kill.Execute(context.Background(), mustJSON(t, processIDArgs{ID: "bg1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "killed") {
		t.Fatalf("expected a kill confirmation, got %q", out)
	}

	waitUntil(t, 6*time.Second, func() bool {
		return !pidAlive(childPid)
	})
}

func TestKillAllStopsEverything(t *testing.T) {
	pm := newProcessManager(t.TempDir())
	run := newRunBackground(pm)
	_, _ = run.Execute(context.Background(), mustJSON(t, runBackgroundArgs{Command: "sleep 30"}))
	_, _ = run.Execute(context.Background(), mustJSON(t, runBackgroundArgs{Command: "sleep 30"}))

	waitUntil(t, time.Second, func() bool { return len(pm.list()) == 2 })

	pm.killAll()

	waitUntil(t, time.Second, func() bool {
		for _, p := range pm.list() {
			if _, running, _, _ := p.snapshot(); running {
				return false
			}
		}
		return true
	})
}

func TestBackgroundOutputBufferIsCapped(t *testing.T) {
	p := &backgroundProcess{}
	big := make([]byte, backgroundMaxOutputBytes+1000)
	for i := range big {
		big[i] = 'x'
	}
	_, _ = p.write(big)

	output, _, _, _ := p.snapshot()
	if len(output) > backgroundMaxOutputBytes {
		t.Errorf("buffered output len = %d, want capped at %d", len(output), backgroundMaxOutputBytes)
	}
}

func TestBackgroundOutputCursorInitialAppendAndStaleReset(t *testing.T) {
	p := &backgroundProcess{id: "bg7", command: "worker", running: true, started: time.Now()}
	_, _ = p.write([]byte("hello"))

	initial := p.outputSince(nil)
	if initial.Output != "hello" || initial.Cursor != 5 || !initial.Reset || !initial.Running {
		t.Fatalf("initial snapshot = %+v", initial)
	}
	_, _ = p.write([]byte(" world"))
	cursor := initial.Cursor
	appendOnly := p.outputSince(&cursor)
	if appendOnly.Output != " world" || appendOnly.Cursor != 11 || appendOnly.Reset {
		t.Fatalf("incremental snapshot = %+v", appendOnly)
	}

	stale := int64(999) // e.g. a cursor retained across a daemon restart
	reset := p.outputSince(&stale)
	if reset.Output != "hello world" || !reset.Reset {
		t.Fatalf("stale cursor snapshot = %+v, want a full reset", reset)
	}
}

func TestBackgroundOutputSnapshotBoundsLargeGapsAndReportsTruncation(t *testing.T) {
	p := &backgroundProcess{id: "bg1", running: true, started: time.Now()}
	big := strings.Repeat("x", backgroundMaxOutputBytes+1000)
	_, _ = p.write([]byte(big))

	info := p.info()
	if info.OutputBytes != int64(len(big)) || info.RetainedBytes != backgroundMaxOutputBytes || !info.Truncated {
		t.Fatalf("info = %+v", info)
	}
	output := p.outputSince(nil)
	if len(output.Output) != backgroundOutputChunkBytes || !output.Reset || !output.Truncated {
		t.Fatalf("bounded output = len %d, reset=%v truncated=%v", len(output.Output), output.Reset, output.Truncated)
	}
}

func TestStartedBackgroundProcessIDOnlyAcceptsStableSuccessShape(t *testing.T) {
	if got := StartedBackgroundProcessID("started bg25 (pid 42): rails server"); got != "bg25" {
		t.Fatalf("ID = %q, want bg25", got)
	}
	for _, invalid := range []string{"", "error: failed", "started nope", "prefix started bg1"} {
		if got := StartedBackgroundProcessID(invalid); got != "" {
			t.Errorf("StartedBackgroundProcessID(%q) = %q, want empty", invalid, got)
		}
	}
}

func TestRegistryBackgroundObserverIsReadOnlyAndHandlesUnknownID(t *testing.T) {
	registry := NewRegistry(t.TempDir(), nil, nil)
	t.Cleanup(registry.StopBackgroundProcesses)
	if _, err := registry.Execute(context.Background(), "run_background", json.RawMessage(`{"command":"printf registry-output"}`)); err != nil {
		t.Fatal(err)
	}

	var processes []BackgroundProcessInfo
	var output BackgroundProcessOutput
	waitUntil(t, 2*time.Second, func() bool {
		processes = registry.BackgroundProcesses()
		var ok bool
		output, ok = registry.BackgroundProcessOutput("bg1", nil)
		return ok && strings.Contains(output.Output, "registry-output")
	})
	if len(processes) != 1 || processes[0].PID <= 0 || processes[0].Command != "printf registry-output" {
		t.Fatalf("process metadata = %+v", processes)
	}
	if _, ok := registry.BackgroundProcessOutput("bg404", nil); ok {
		t.Fatal("unknown process unexpectedly produced output")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// readPidFile waits briefly for a PID file a background shell command
// writes asynchronously (via `echo $!`) and parses it — the write can
// lag slightly behind run_background's own "started" response.
func readPidFile(t *testing.T, path string) int {
	t.Helper()
	var data []byte
	waitUntil(t, 2*time.Second, func() bool {
		b, err := os.ReadFile(path)
		if err != nil || len(b) == 0 {
			return false
		}
		data = b
		return true
	})
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing pid file %s: %v", path, err)
	}
	return pid
}

// pidAlive reports whether a process with the given pid still exists,
// using signal 0 (POSIX-guaranteed to do existence/permission checking
// only — no signal actually delivered). This is the deterministic check
// the mission calls for in place of scraping `ps` output.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
