package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBashRunsSimpleCommand(t *testing.T) {
	b := &bash{workspace: t.TempDir()}
	out, err := b.Execute(context.Background(), mustJSON(t, bashArgs{Command: "echo hello-from-bash"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello-from-bash") {
		t.Errorf("expected stdout to contain the echoed text, got %q", out)
	}
}

func TestBashExitCodeFraming(t *testing.T) {
	b := &bash{workspace: t.TempDir()}
	out, err := b.Execute(context.Background(), mustJSON(t, bashArgs{Command: "exit 7"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[exit code 7]") {
		t.Errorf("expected non-editorializing exit code framing, got %q", out)
	}
	if strings.Contains(out, "error") {
		t.Errorf("a plain non-zero exit should not say 'error' — see DECISIONS.md, got %q", out)
	}
}

func TestBashTimeoutIsReported(t *testing.T) {
	b := &bash{workspace: t.TempDir()}
	out, err := b.Execute(context.Background(), mustJSON(t, bashArgs{Command: "sleep 5", TimeoutSeconds: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected a timeout message, got %q", out)
	}
}

func TestBashCombinesStdoutAndStderr(t *testing.T) {
	b := &bash{workspace: t.TempDir()}
	out, err := b.Execute(context.Background(), mustJSON(t, bashArgs{Command: "echo out-line; echo err-line 1>&2"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "out-line") || !strings.Contains(out, "err-line") {
		t.Errorf("expected both stdout and stderr captured, got %q", out)
	}
}

// TestBashTimeoutKillsWholeProcessTree is the critical regression test for
// the bug this package's shell runner exists to fix: a naive
// exec.CommandContext("sh", "-c", ...) only kills the shell process when
// its context expires, leaving any child the shell forked (here, a
// backgrounded `sleep`) running as an orphan. bash's timeout must kill
// the whole tree, the same way process_kill does for run_background (see
// TestProcessKillTerminatesChildProcessToo in background_test.go).
func TestBashTimeoutKillsWholeProcessTree(t *testing.T) {
	dir := t.TempDir()
	childPidFile := dir + "/child.pid"

	b := &bash{workspace: dir}
	_, err := b.Execute(context.Background(), mustJSON(t, bashArgs{
		Command:        "sleep 30 & echo $! > " + childPidFile + "; wait",
		TimeoutSeconds: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}

	childPid := readPidFile(t, childPidFile)

	waitUntil(t, 6*time.Second, func() bool {
		return !pidAlive(childPid)
	})
}

func TestBashInvalidArguments(t *testing.T) {
	b := &bash{workspace: t.TempDir()}
	out, err := b.Execute(context.Background(), json.RawMessage(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("expected an error message for invalid arguments, got %q", out)
	}
}

func TestBashEmptyCommandRejected(t *testing.T) {
	b := &bash{workspace: t.TempDir()}
	out, err := b.Execute(context.Background(), mustJSON(t, bashArgs{Command: ""}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("expected an error message for an empty command, got %q", out)
	}
}
