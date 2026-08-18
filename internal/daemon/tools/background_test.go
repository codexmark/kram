package tools

import (
	"context"
	"encoding/json"
	"strings"
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

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
