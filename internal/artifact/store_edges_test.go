package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCorruptMetadataMissingContentAndClampDefaults(t *testing.T) {
	s := Open(t.TempDir())
	a, err := s.Save("s", "c", "tool", strings.NewReader(strings.Repeat("x", defaultReadLimit+20)))
	if err != nil {
		t.Fatal(err)
	}
	content, meta, err := s.Read(a.ID, -50, defaultReadLimit+500)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != defaultReadLimit || meta.ID != a.ID {
		t.Fatalf("len=%d meta=%#v", len(content), meta)
	}
	content, _, err = s.Read(a.ID, 0, 0)
	if err != nil || len(content) != defaultReadLimit {
		t.Fatalf("default len=%d err=%v", len(content), err)
	}
	contentPath, metaPath, err := s.paths(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(a.ID, 0, 1); err == nil || !strings.Contains(err.Error(), "corrupt metadata") {
		t.Fatalf("err=%v", err)
	}
	if err := os.WriteFile(metaPath, []byte(`{"id":"`+a.ID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(contentPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(a.ID, 0, 1); err == nil || !strings.Contains(err.Error(), "no such artifact") {
		t.Fatalf("err=%v", err)
	}
}

func TestSaveAndSaveFileFilesystemErrors(t *testing.T) {
	if _, err := Open(t.TempDir()).SaveFile("", "", "", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing source should fail")
	}
	root := t.TempDir()
	block := filepath.Join(root, ".kram")
	if err := os.WriteFile(block, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root).Save("", "", "", strings.NewReader("x")); err == nil {
		t.Fatal("mkdir through file should fail")
	}
	w := NewSpillWriter(10)
	if w.TempPath() != "" || w.Spilled() {
		t.Fatal("new writer should be memory-only")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
