package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This is the regression test for a real bug: grep walked into .kram
// (the daemon's own live SQLite database) and returned binary garbage as
// search matches, which is exactly the kind of confusing tool result
// that can derail a weak model's next turn (see DECISIONS.md).

func TestGrepSkipsKramDir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "real.go"), "// TODO fix this\npackage main\n")
	mustWrite(t, filepath.Join(dir, ".kram", "kram-daemon.db"), "TODO\x00\x01\x02binary garbage TODO")

	g := newGrep(dir)
	out := runGrep(t, g, "TODO", "")

	if !strings.Contains(out, "real.go") {
		t.Errorf("expected a match in real.go, got:\n%s", out)
	}
	if strings.Contains(out, "kram-daemon.db") || strings.Contains(out, ".kram") {
		t.Errorf(".kram should never be searched, got:\n%s", out)
	}
}

func TestGrepSkipsBinaryFilesOutsideKram(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "notes.txt"), "TODO write docs\n")
	mustWrite(t, filepath.Join(dir, "asset.bin"), "TODO\x00\x00\x00binary")

	g := newGrep(dir)
	out := runGrep(t, g, "TODO", "")

	if !strings.Contains(out, "notes.txt") {
		t.Errorf("expected a match in notes.txt, got:\n%s", out)
	}
	if strings.Contains(out, "asset.bin") {
		t.Errorf("a file with NUL bytes should be treated as binary and skipped, got:\n%s", out)
	}
}

func TestGrepFindsOrdinaryMatches(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "func Foo() {}\nfunc Bar() {}\n")

	g := newGrep(dir)
	out := runGrep(t, g, "func Foo", "")

	if !strings.Contains(out, "a.go:1:") {
		t.Errorf("expected a match at a.go:1, got:\n%s", out)
	}
	if strings.Contains(out, "Bar") {
		t.Errorf("should not match Bar, got:\n%s", out)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGrep(t *testing.T, g *grep, pattern, path string) string {
	t.Helper()
	args, err := json.Marshal(grepArgs{Pattern: pattern, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
