package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codexmark/kram/internal/shell"
)

// backgroundMaxOutputBytes caps how much output one process buffers —
// larger than bash's single-call cap (50KB) since a background process
// is expected to run far longer and accumulate far more, but still
// bounded: an unbounded buffer on a long-lived dev server's stdout is a
// slow memory leak.
const backgroundMaxOutputBytes = 500_000

// backgroundOutputChunkBytes bounds one UI/API read. The process buffer is
// deliberately larger so a user can open the panel late and still inspect a
// useful tail, but repeatedly shipping all 500KB over localhost would turn a
// cheap status view into needless allocation and JSON work.
const backgroundOutputChunkBytes = 64 * 1024

// BackgroundProcessInfo is the read-only, structured view of one process
// owned by this daemon. It intentionally contains no os.Process or command
// handle: callers can observe lifecycle and output without acquiring a second
// way to mutate process state outside the permission-gated tools.
type BackgroundProcessInfo struct {
	ID            string     `json:"id"`
	Command       string     `json:"command"`
	PID           int        `json:"pid"`
	Running       bool       `json:"running"`
	ExitCode      int        `json:"exit_code"`
	ExitError     string     `json:"exit_error,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	OutputBytes   int64      `json:"output_bytes"`
	RetainedBytes int        `json:"retained_bytes"`
	Truncated     bool       `json:"truncated"`
}

// BackgroundProcessOutput is one cursor-based read of captured stdout and
// stderr. Cursor is an absolute byte position in this process's output stream.
// Reset tells a client to replace, rather than append to, its local copy.
type BackgroundProcessOutput struct {
	ID        string `json:"id"`
	Output    string `json:"output"`
	Cursor    int64  `json:"cursor"`
	Reset     bool   `json:"reset"`
	Running   bool   `json:"running"`
	ExitCode  int    `json:"exit_code"`
	ExitError string `json:"exit_error,omitempty"`
	Truncated bool   `json:"truncated"`
}

// backgroundProcess is one long-running command, tracked from start
// until the caller kills it or it exits on its own. Unlike bash (which
// is foreground-only and timeout-bounded by design — see DECISIONS.md),
// this is Kram's answer to "start a dev server and keep working": the
// process genuinely outlives the tool call that started it.
type backgroundProcess struct {
	id      string
	command string
	cmd     *exec.Cmd

	mu       sync.Mutex
	output   bytes.Buffer
	running  bool
	exitCode int
	exitErr  string
	started  time.Time
	ended    time.Time
	totalOut int64
}

func (p *backgroundProcess) write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalOut += int64(len(b))
	p.output.Write(b)
	if p.output.Len() > backgroundMaxOutputBytes {
		// Keep the tail — the most recent output is almost always what
		// matters for a long-running process (a server's latest request
		// log, a build's latest progress), not what it printed at start.
		trimmed := p.output.Bytes()
		trimmed = trimmed[len(trimmed)-backgroundMaxOutputBytes:]
		p.output.Reset()
		p.output.Write(trimmed)
	}
	return len(b), nil
}

func (p *backgroundProcess) info() BackgroundProcessInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	pid := 0
	if p.cmd != nil && p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}
	var endedAt *time.Time
	if !p.ended.IsZero() {
		ended := p.ended
		endedAt = &ended
	}
	retained := p.output.Len()
	return BackgroundProcessInfo{
		ID: p.id, Command: p.command, PID: pid, Running: p.running,
		ExitCode: p.exitCode, ExitError: p.exitErr, StartedAt: p.started,
		EndedAt: endedAt, OutputBytes: p.totalOut, RetainedBytes: retained,
		Truncated: p.totalOut > int64(retained),
	}
}

func (p *backgroundProcess) outputSince(cursor *int64) BackgroundProcessOutput {
	p.mu.Lock()
	defer p.mu.Unlock()

	data := p.output.Bytes()
	end := p.totalOut
	retainedStart := end - int64(len(data))
	start := retainedStart
	reset := true // a missing cursor is an initial snapshot, never an append
	if cursor != nil && *cursor >= retainedStart && *cursor <= end {
		start = *cursor
		reset = false
	}
	if end-start > backgroundOutputChunkBytes {
		start = end - backgroundOutputChunkBytes
		reset = true
	}
	index := int(start - retainedStart)
	// A byte cursor can land inside a multi-byte rune after truncation. Move
	// forward to the next complete rune so the UI never displays a replacement
	// glyph merely because a bounded read split one character.
	for index < len(data) && !utf8.RuneStart(data[index]) {
		index++
		start++
	}

	return BackgroundProcessOutput{
		ID: p.id, Output: string(data[index:]), Cursor: end, Reset: reset,
		Running: p.running, ExitCode: p.exitCode, ExitError: p.exitErr,
		Truncated: retainedStart > 0 || start > retainedStart,
	}
}

func (p *backgroundProcess) snapshot() (output string, running bool, exitCode int, exitErr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.String(), p.running, p.exitCode, p.exitErr
}

// processManager owns every background process for one daemon —
// deliberately daemon-lifetime, not session-lifetime, matching todoStore:
// a dev server started from one session should still be checkable (and
// killable) from a different session, or after resuming the same one
// later.
type processManager struct {
	workspace string
	mu        sync.Mutex
	procs     map[string]*backgroundProcess
	nextID    int
}

func newProcessManager(workspace string) *processManager {
	return &processManager{workspace: workspace, procs: make(map[string]*backgroundProcess)}
}

func (m *processManager) start(command string) (*backgroundProcess, error) {
	m.mu.Lock()
	m.nextID++
	id := "bg" + strconv.Itoa(m.nextID)
	m.mu.Unlock()

	// Background processes are daemon-lifetime, not request-lifetime (see
	// DECISIONS.md) — there's no per-call context to bound them with, so
	// this uses context.Background() rather than a timeout. Going through
	// shell.Command still matters here: it's what makes process-tree
	// cleanup (killAll, process_kill) reach children the command spawns,
	// not just the shell itself.
	cmd := shell.Command(context.Background(), m.workspace, command)

	p := &backgroundProcess{id: id, command: command, cmd: cmd, running: true, started: time.Now()}
	cmd.Stdout = writerFunc(p.write)
	cmd.Stderr = writerFunc(p.write)

	if err := shell.Start(cmd); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.procs[id] = p
	m.mu.Unlock()

	go func() {
		waitErr := shell.Wait(cmd)
		p.mu.Lock()
		p.running = false
		p.ended = time.Now()
		p.exitCode = cmd.ProcessState.ExitCode()
		if waitErr != nil {
			p.exitErr = waitErr.Error()
		}
		p.mu.Unlock()
	}()

	return p, nil
}

func (m *processManager) get(id string) (*backgroundProcess, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[id]
	return p, ok
}

func (m *processManager) list() []*backgroundProcess {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*backgroundProcess, 0, len(m.procs))
	for _, p := range m.procs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].started.Before(out[j].started) })
	return out
}

func (m *processManager) infos() []BackgroundProcessInfo {
	procs := m.list()
	out := make([]BackgroundProcessInfo, 0, len(procs))
	for _, p := range procs {
		out = append(out, p.info())
	}
	return out
}

func (m *processManager) output(id string, cursor *int64) (BackgroundProcessOutput, bool) {
	p, ok := m.get(id)
	if !ok {
		return BackgroundProcessOutput{}, false
	}
	return p.outputSince(cursor), true
}

// killAll is called on daemon shutdown so a background dev server doesn't
// outlive the daemon that started it as an orphaned, untracked process.
// Goes through shell.KillTree rather than Process.Kill() so a tracked
// process's children (a `npm start` that forked `node`, say) are killed
// too, not just the shell that was directly started.
func (m *processManager) killAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		if p.cmd.Process != nil {
			_ = shell.KillTree(p.cmd)
		}
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// --- tools ---

type runBackground struct{ procs *processManager }

func newRunBackground(pm *processManager) *runBackground { return &runBackground{procs: pm} }

func (t *runBackground) Name() string { return "run_background" }
func (t *runBackground) Description() string {
	return "Start a long-running shell command (a dev server, a watcher, a build daemon) that keeps running after this tool call returns — unlike bash, which is foreground-only and bounded by a timeout. Returns a process id; use process_output to read what it's printed, process_list to see everything running, and process_kill to stop it."
}
func (t *runBackground) ToolMetadata() ToolMetadata {
	return ToolMetadata{
		Summary:    "A dev server, watcher, or build daemon; check it with process_output, stop it with process_kill.",
		PreferOver: "bash",
	}
}
func (t *runBackground) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "The shell command to run in the background."}
		},
		"required": ["command"]
	}`)
}

type runBackgroundArgs struct {
	Command string `json:"command"`
}

func (t *runBackground) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args runBackgroundArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.Command == "" {
		return "error: command must not be empty", nil
	}
	p, err := t.procs.start(args.Command)
	if err != nil {
		return fmt.Sprintf("error: failed to start: %v", err), nil
	}
	return fmt.Sprintf("started %s (pid %d): %s", p.id, p.cmd.Process.Pid, args.Command), nil
}

// StartedBackgroundProcessID extracts the ID from run_background's own stable
// success result. Keeping this parser next to the producer lets the agent add
// click metadata without teaching the TUI to scrape human-facing text.
func StartedBackgroundProcessID(result string) string {
	fields := strings.Fields(result)
	if len(fields) < 2 || fields[0] != "started" || !strings.HasPrefix(fields[1], "bg") {
		return ""
	}
	if _, err := strconv.Atoi(strings.TrimPrefix(fields[1], "bg")); err != nil {
		return ""
	}
	return fields[1]
}

type processList struct{ procs *processManager }

func newProcessList(pm *processManager) *processList { return &processList{procs: pm} }

func (t *processList) Name() string { return "process_list" }
func (t *processList) Description() string {
	return "List every background process started with run_background in this daemon, running or finished, with its id and status."
}
func (t *processList) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}
func (t *processList) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	procs := t.procs.list()
	if len(procs) == 0 {
		return "(no background processes)", nil
	}
	var b strings.Builder
	for _, p := range procs {
		_, running, exitCode, exitErr := p.snapshot()
		status := fmt.Sprintf("exited %d", exitCode)
		if running {
			status = "running"
		} else if exitErr != "" {
			status = exitErr
		}
		fmt.Fprintf(&b, "%s [%s]: %s\n", p.id, status, p.command)
	}
	return b.String(), nil
}

type processOutput struct{ procs *processManager }

func newProcessOutput(pm *processManager) *processOutput { return &processOutput{procs: pm} }

func (t *processOutput) Name() string { return "process_output" }
func (t *processOutput) Description() string {
	return "Read a background process's captured stdout/stderr so far, by id (see process_list). Safe to call repeatedly while it's still running."
}
func (t *processOutput) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Process id, as returned by run_background or listed by process_list."}
		},
		"required": ["id"]
	}`)
}

type processIDArgs struct {
	ID string `json:"id"`
}

func (t *processOutput) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args processIDArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	p, ok := t.procs.get(args.ID)
	if !ok {
		return fmt.Sprintf("error: no background process with id %q", args.ID), nil
	}
	output, running, exitCode, exitErr := p.snapshot()
	status := fmt.Sprintf("[exited %d]", exitCode)
	if running {
		status = "[still running]"
	} else if exitErr != "" {
		status = fmt.Sprintf("[%s]", exitErr)
	}
	if output == "" {
		return status + " (no output yet)", nil
	}
	return output + "\n\n" + status, nil
}

type processKill struct{ procs *processManager }

func newProcessKill(pm *processManager) *processKill { return &processKill{procs: pm} }

func (t *processKill) Name() string { return "process_kill" }
func (t *processKill) Description() string {
	return "Stop a background process by id (see process_list)."
}
func (t *processKill) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Process id to stop."}
		},
		"required": ["id"]
	}`)
}
func (t *processKill) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args processIDArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	p, ok := t.procs.get(args.ID)
	if !ok {
		return fmt.Sprintf("error: no background process with id %q", args.ID), nil
	}
	_, running, _, _ := p.snapshot()
	if !running {
		return fmt.Sprintf("%s already exited", args.ID), nil
	}
	if p.cmd.Process == nil {
		return "error: process has no PID yet", nil
	}
	// KillTree, not Process.Kill(): a background command that forked
	// children (a dev server's own worker processes, a backgrounded
	// `sleep &`) would otherwise leave them running as orphans after
	// "killing" the process that's tracked here.
	if err := shell.KillTree(p.cmd); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("killed %s", args.ID), nil
}
