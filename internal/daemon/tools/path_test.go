package tools

import (
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
