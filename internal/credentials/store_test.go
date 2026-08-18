package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

// isolate points XDG_CONFIG_HOME at a fresh temp dir for the duration of
// one test — the same isolation this package needed by hand (via env -u)
// during manual testing, now automatic. Without it, a test touching this
// package risks reading or writing the real user's config.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load on a fresh dir should not error, got: %v", err)
	}
	if got := s.Get("ANTHROPIC_API_KEY"); got != "" {
		t.Errorf("expected empty string for an unset key, got %q", got)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("ANTHROPIC_API_KEY", "sk-test-123"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("ANTHROPIC_API_KEY"); got != "sk-test-123" {
		t.Errorf("got %q, want sk-test-123", got)
	}

	// A fresh Load must see what a previous Store instance persisted.
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get("ANTHROPIC_API_KEY"); got != "sk-test-123" {
		t.Errorf("after reload, got %q, want sk-test-123", got)
	}
}

func TestDelete(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.Set("OPENAI_API_KEY", "sk-abc")
	if err := s.Delete("OPENAI_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("OPENAI_API_KEY"); got != "" {
		t.Errorf("expected empty after delete, got %q", got)
	}

	reloaded, _ := Load()
	if got := reloaded.Get("OPENAI_API_KEY"); got != "" {
		t.Errorf("delete should persist — after reload got %q", got)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := isolate(t)
	s, _ := Load()
	if err := s.Set("GEMINI_API_KEY", "sk-xyz"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "kram-gateway", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("credentials file should exist at %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials file permissions = %o, want 600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("credentials dir permissions = %o, want 700", perm)
	}
}

func TestAllReturnsACopy(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.Set("ANTHROPIC_API_KEY", "sk-1")

	all := s.All()
	all["ANTHROPIC_API_KEY"] = "tampered"

	if got := s.Get("ANTHROPIC_API_KEY"); got != "sk-1" {
		t.Errorf("mutating the map from All() should not affect the store, got %q", got)
	}
}
