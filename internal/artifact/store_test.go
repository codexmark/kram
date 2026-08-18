package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveAndReadRoundTrip(t *testing.T) {
	s := Open(t.TempDir())
	a, err := s.Save("sess1", "call1", "bash", strings.NewReader("hello, artifact"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Size != int64(len("hello, artifact")) {
		t.Errorf("Size = %d, want %d", a.Size, len("hello, artifact"))
	}
	if a.SHA256 == "" {
		t.Error("expected a non-empty sha256")
	}

	text, meta, err := s.Read(a.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello, artifact" {
		t.Errorf("Read = %q", text)
	}
	if meta.ToolName != "bash" || meta.SessionID != "sess1" {
		t.Errorf("unexpected metadata: %+v", meta)
	}
}

func TestReadOffsetAndLimit(t *testing.T) {
	s := Open(t.TempDir())
	a, err := s.Save("", "", "x", strings.NewReader("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := s.Read(a.ID, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if text != "3456" {
		t.Errorf("Read(offset=3, limit=4) = %q, want %q", text, "3456")
	}
}

func TestReadNonexistentArtifact(t *testing.T) {
	s := Open(t.TempDir())
	if _, _, err := s.Read("art_0000000000000000", 0, 0); err == nil {
		t.Error("expected an error for a nonexistent artifact id")
	}
}

func TestReadRejectsMalformedID(t *testing.T) {
	s := Open(t.TempDir())
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", "art_short", "", "art_ZZZZZZZZZZZZZZZZ"} {
		if _, _, err := s.Read(bad, 0, 0); err == nil {
			t.Errorf("expected Read(%q) to reject a malformed id, got no error", bad)
		}
	}
}

func TestReadNeverEscapesArtifactsDir(t *testing.T) {
	workspace := t.TempDir()
	s := Open(workspace)
	// A path-traversal id must never resolve outside .kram/artifacts, even
	// if such a file happened to exist one level up.
	secret := filepath.Join(workspace, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read("../secret", 0, 0); err == nil {
		t.Fatal("path-traversal-shaped id must be rejected")
	}
}

func TestSaveFileConsumesSource(t *testing.T) {
	s := Open(t.TempDir())
	tmp, err := os.CreateTemp("", "kram-spill-test-*")
	if err != nil {
		t.Fatal(err)
	}
	tmp.WriteString("spilled content")
	tmp.Close()

	a, err := s.SaveFile("sess", "call", "bash", tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp.Name()); !os.IsNotExist(err) {
		t.Error("SaveFile should remove the source file on success")
	}
	text, _, err := s.Read(a.ID, 0, 0)
	if err != nil || text != "spilled content" {
		t.Errorf("text=%q err=%v", text, err)
	}
}

func TestPreview(t *testing.T) {
	s := Open(t.TempDir())
	a, _ := s.Save("", "", "x", strings.NewReader(strings.Repeat("ab", 100)))
	preview, err := s.Preview(a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 10 {
		t.Errorf("Preview length = %d, want 10", len(preview))
	}
}

func TestGCRemovesOldArtifactsOnly(t *testing.T) {
	workspace := t.TempDir()
	s := Open(workspace)
	old, err := s.Save("", "", "x", strings.NewReader("old"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Save("", "", "x", strings.NewReader("fresh"))
	if err != nil {
		t.Fatal(err)
	}

	oldMetaPath := filepath.Join(workspace, ".kram", "artifacts", old.ID+".json")
	longAgo := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldMetaPath, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}

	s.GC(24 * time.Hour)

	if _, _, err := s.Read(old.ID, 0, 0); err == nil {
		t.Error("expected the old artifact to be garbage collected")
	}
	if _, _, err := s.Read(fresh.ID, 0, 0); err != nil {
		t.Errorf("the fresh artifact should have survived GC, got error: %v", err)
	}
}

func TestGCOnMissingDirIsNoop(t *testing.T) {
	s := Open(t.TempDir()) // .kram/artifacts never created
	s.GC(time.Hour)        // must not panic or error
}
