//go:build windows

package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

func newShellCmd(ctx context.Context, command string) *exec.Cmd {
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	// /S /C tells cmd.exe to treat everything after /C as one string to
	// run, stripping the outer quotes it would otherwise apply to the
	// whole command line itself — the closest cmd.exe equivalent to
	// `sh -c "<command>"`'s "run this string" semantics. This
	// deliberately does not shell out to sh.exe/bash.exe: those only
	// exist if the user happens to have Git Bash or WSL installed, and
	// silently depending on that was the bug being fixed.
	return exec.CommandContext(ctx, comspec, "/S", "/C", command)
}

// jobs tracks the Job Object each shell.Start'd *exec.Cmd was assigned
// to. Keyed by the *exec.Cmd pointer rather than PID so a reused PID
// (however unlikely) can't cross-wire two unrelated commands, and so
// KillTree/cleanup don't need callers to thread an extra handle through
// bash.go/background.go/customtools.go alongside the *exec.Cmd they
// already pass around.
var jobs sync.Map // *exec.Cmd -> windows.Handle

// afterStart assigns the just-started process to a new Job Object
// configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so terminating (or
// even just closing) the job kills every process in it. Windows has no
// process-group/SIGTERM-to-a-group equivalent the way Unix does — a Job
// Object is the actual OS mechanism for "kill this and everything it
// started" (the same one Docker Desktop, containerd, and Chromium's
// process launcher use on this platform).
//
// Known limitation: the process is assigned to the job after it's
// already running, not created suspended into it, so a child that forks
// in the brief window between Start and this call could escape the job.
// Closing that window would mean creating the process with
// CREATE_SUSPENDED and resuming it after assignment, which needs the
// thread handle from CreateProcess's PROCESS_INFORMATION — os/exec
// doesn't expose it, so doing this properly would mean reimplementing
// CreateProcess ourselves instead of using os/exec. Accepted as a
// practical tradeoff: shell commands spawn children well after cmd.exe
// itself starts running (parsing the command line, resolving the
// program), not in that first sliver of time.
func afterStart(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("shell: create job object: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("shell: configure job object: %w", err)
	}

	proc, err := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("shell: open process: %w", err)
	}
	defer windows.CloseHandle(proc)

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("shell: assign process to job object: %w", err)
	}

	jobs.Store(cmd, job)
	return nil
}

// cleanup releases the Job Object handle once a process has exited on
// its own (rather than via KillTree) — without this, every
// run_background command that finishes normally leaks one OS handle for
// the life of the daemon.
func cleanup(cmd *exec.Cmd) {
	if v, ok := jobs.LoadAndDelete(cmd); ok {
		windows.CloseHandle(v.(windows.Handle))
	}
}

// killTree terminates every process in cmd's Job Object. LoadAndDelete
// makes this safe to race against cleanup (a process that happens to
// exit on its own at the same moment KillTree is called): whichever one
// gets the handle first does the closing, the other becomes a no-op.
//
// If no job was ever registered — Start's afterStart failed, or this is
// racing a cleanup that already ran — this falls back to killing just
// the one process it still knows about. Not a full tree kill, but
// strictly better than doing nothing.
func killTree(cmd *exec.Cmd) error {
	v, ok := jobs.LoadAndDelete(cmd)
	if !ok {
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	job := v.(windows.Handle)
	terminateErr := windows.TerminateJobObject(job, 1)
	windows.CloseHandle(job)
	return terminateErr
}

func describe() string {
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	return comspec + " /S /C"
}
