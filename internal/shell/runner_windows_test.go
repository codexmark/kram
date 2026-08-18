//go:build windows

// This file only needs to COMPILE in this development environment
// (Linux, cross-compiling) — it cannot actually run here. It was
// verified with `GOOS=windows GOARCH=amd64 go vet ./...`, not
// `go test`. Whoever next has access to a real Windows machine (or CI
// running windows-latest) should run this file for real and delete this
// notice once it's been confirmed passing there.
package shell

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWindowsCommandUsesComspec(t *testing.T) {
	t.Setenv("COMSPEC", `C:\Windows\System32\cmd.exe`)
	cmd := Command(context.Background(), t.TempDir(), "echo hello")
	if cmd.Path != `C:\Windows\System32\cmd.exe` && !strings.HasSuffix(cmd.Path, "cmd.exe") {
		t.Errorf("expected the command to resolve via COMSPEC, got Path=%q", cmd.Path)
	}
	found := false
	for _, a := range cmd.Args {
		if a == "/S" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected /S among args, got %v", cmd.Args)
	}
}

func TestWindowsCommandFallsBackToLiteralCmdExe(t *testing.T) {
	t.Setenv("COMSPEC", "")
	os.Unsetenv("COMSPEC")
	cmd := Command(context.Background(), t.TempDir(), "echo hello")
	if !strings.Contains(cmd.Path, "cmd.exe") && cmd.Path != "cmd.exe" {
		t.Errorf("expected a fallback to the literal cmd.exe, got Path=%q", cmd.Path)
	}
}

func TestWindowsRunAndKillTreeCompileAndRun(t *testing.T) {
	// A real run: cmd.exe should exist on any Windows CI runner. This
	// exercises Start -> Job Object assignment -> Wait -> cleanup for a
	// command that exits on its own.
	cmd := Command(context.Background(), t.TempDir(), "echo hi")
	if err := Run(cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWindowsKillTreeStopsLongRunningProcess(t *testing.T) {
	cmd := Command(context.Background(), t.TempDir(), "ping -n 30 127.0.0.1 >NUL")
	if err := Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- Wait(cmd) }()

	// Give the process a moment to actually start before killing it.
	time.Sleep(200 * time.Millisecond)
	if err := KillTree(cmd); err != nil {
		t.Fatalf("KillTree: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process was not terminated by KillTree within 5s")
	}
}

func TestWindowsDescribeMentionsComspec(t *testing.T) {
	if !strings.Contains(Describe(), "cmd.exe") {
		t.Errorf("expected Describe() to mention cmd.exe, got %q", Describe())
	}
}
