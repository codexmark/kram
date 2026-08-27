package localstore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteCreatesFileAndParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "data.json")

	if err := AtomicWrite(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
}

func TestAtomicWriteAppliesPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file mode bits aren't meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	if err := AtomicWrite(path, []byte("k"), 0o600); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
}

func TestAtomicWriteOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data")

	if err := AtomicWrite(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := AtomicWrite(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("content = %q, want %q", got, "second")
	}
}

// TestAtomicWriteLeavesNoTempOnSuccess guards the property callers rely on:
// after a successful write the sibling ".tmp" file is gone (renamed into
// place), never left behind to confuse a later reader or a directory scan.
func TestAtomicWriteLeavesNoTempOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data")

	if err := AtomicWrite(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file still present after success (stat err = %v)", err)
	}
}
