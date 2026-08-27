package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathAcceptsAbsolutePathInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	want := filepath.Join(workspace, "app", "models", "student.rb")

	got, err := resolvePath(workspace, want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolvePath() = %q, want %q", got, want)
	}
}

func TestResolvePathStillRejectsEscapes(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(filepath.Dir(workspace), "outside")

	for _, path := range []string{"../outside", outside, workspace + "-sibling"} {
		if got, err := resolvePath(workspace, path); err == nil {
			t.Errorf("resolvePath(%q) = %q, want workspace escape error", path, got)
		}
	}
}

func TestResolvePathKeepsRelativeAndEmptyPathsInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	tests := map[string]string{
		"":               workspace,
		"app/models":     filepath.Join(workspace, "app", "models"),
		"app/../Gemfile": filepath.Join(workspace, "Gemfile"),
	}
	for input, want := range tests {
		got, err := resolvePath(workspace, input)
		if err != nil {
			t.Errorf("resolvePath(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("resolvePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolvePathRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir() // a real directory outside the workspace
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("stolen"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink INSIDE the workspace pointing OUT — the exact shape a
	// cloned repo carrying `link -> ~/.ssh` would have.
	link := filepath.Join(workspace, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	// Reading an existing file through the symlink must be rejected...
	if got, err := resolvePath(workspace, "escape/secret.txt"); err == nil {
		t.Errorf("resolvePath(escape/secret.txt) = %q, want a symlink-escape error", got)
	}
	// ...and so must creating a NEW file through it (the write_file case,
	// where the final component doesn't exist yet but the symlinked parent
	// does).
	if got, err := resolvePath(workspace, "escape/newfile.txt"); err == nil {
		t.Errorf("resolvePath(escape/newfile.txt) = %q, want a symlink-escape error", got)
	}
}

func TestResolvePathAllowsSymlinkStayingInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	realDir := filepath.Join(workspace, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink that stays inside the workspace is fine.
	link := filepath.Join(workspace, "alias")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	if _, err := resolvePath(workspace, "alias/file.txt"); err != nil {
		t.Errorf("resolvePath(alias/file.txt) rejected an in-workspace symlink: %v", err)
	}
}
