package customprovider

import (
	"os"
	"path/filepath"
	"testing"
)

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
	if got := s.All(); len(got) != 0 {
		t.Errorf("expected no entries, got %d", len(got))
	}
}

func TestAddGetRoundTrip(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Add("Meu Servidor", "http://192.168.1.50:8080/v1", "llama-3", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "meu-servidor" {
		t.Errorf("ID = %q, want meu-servidor", p.ID)
	}
	if p.EnvVar != "CUSTOM_MEU_SERVIDOR_API_KEY" {
		t.Errorf("EnvVar = %q, want CUSTOM_MEU_SERVIDOR_API_KEY", p.EnvVar)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	all := reloaded.All()
	if len(all) != 1 || all[0].Name != "Meu Servidor" || all[0].BaseURL != "http://192.168.1.50:8080/v1" || all[0].Model != "llama-3" {
		t.Errorf("after reload, got %+v", all)
	}
}

// TestAddPersistsContextWindow confirms the optional per-model window
// survives a save/reload, so a local model's real window reaches the
// compaction budget instead of defaulting to zero (unknown).
func TestAddPersistsContextWindow(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("Lab", "http://127.0.0.1:1234/v1", "qwen", true, 32768); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	all := reloaded.All()
	if len(all) != 1 || all[0].ContextWindow != 32768 {
		t.Errorf("ContextWindow did not round-trip: got %+v", all)
	}
	// A negative window is clamped to 0 (unknown), never persisted as-is.
	s2, _ := Load()
	p, err := s2.Add("Neg", "http://x", "m", true, -5)
	if err != nil {
		t.Fatal(err)
	}
	if p.ContextWindow != 0 {
		t.Errorf("negative window = %d, want clamped to 0", p.ContextWindow)
	}
}

func TestAddRequiresNameBaseURLAndModel(t *testing.T) {
	isolate(t)
	s, _ := Load()
	if _, err := s.Add("", "http://x", "m", true, 0); err == nil {
		t.Error("expected an error for an empty name")
	}
	if _, err := s.Add("x", "", "m", true, 0); err == nil {
		t.Error("expected an error for an empty base URL")
	}
	if _, err := s.Add("x", "http://x", "", true, 0); err == nil {
		t.Error("expected an error for an empty model — passthrough doesn't actually work (see Provider.Model's doc comment), so this must be rejected, not silently accepted")
	}
}

// TestSupportsToolsDefaultsTrueWhenNeverSet is the migration case: an
// entry from before this field existed (or one loaded from raw JSON
// without the key) must read as true, not false — the field's zero
// value must never silently disable tool calling for an existing
// registration.
func TestSupportsToolsDefaultsTrueWhenNeverSet(t *testing.T) {
	var p Provider
	if !p.SupportsToolsOrDefault() {
		t.Error("a Provider with SupportsTools never set should default to true")
	}
}

func TestSupportsToolsExplicitFalseIsRespected(t *testing.T) {
	isolate(t)
	s, _ := Load()
	p, err := s.Add("NoTools", "http://x", "m", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.SupportsToolsOrDefault() {
		t.Error("explicitly setting supportsTools=false should be respected, not defaulted back to true")
	}

	reloaded, _ := Load()
	if got := reloaded.All()[0]; got.SupportsToolsOrDefault() {
		t.Error("supports_tools=false should survive a reload")
	}
}

func TestDuplicateNamesGetDedupedIDs(t *testing.T) {
	isolate(t)
	s, _ := Load()
	a, err := s.Add("Servidor", "http://a", "m", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Add("Servidor", "http://b", "m", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("expected distinct IDs for two entries named the same, both got %q", a.ID)
	}
	if b.ID != "servidor-2" {
		t.Errorf("second entry's ID = %q, want servidor-2", b.ID)
	}
	// Distinct IDs must also mean distinct EnvVars — otherwise the second
	// entry's key would silently overwrite the first's in credentials.Store.
	if a.EnvVar == b.EnvVar {
		t.Error("expected distinct EnvVars for two distinct entries")
	}
}

func TestSlugifyHandlesSymbolsAndSpaces(t *testing.T) {
	isolate(t)
	s, _ := Load()
	p, err := s.Add("LM Studio (casa) — GPU #1!", "http://x", "m", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "lm-studio-casa-gpu-1" {
		t.Errorf("ID = %q, want lm-studio-casa-gpu-1", p.ID)
	}
}

func TestSlugifyFallsBackWhenNameHasNoAlnum(t *testing.T) {
	isolate(t)
	s, _ := Load()
	p, err := s.Add("!!!", "http://x", "m", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "provider" {
		t.Errorf("ID = %q, want the provider fallback", p.ID)
	}
}

func TestDelete(t *testing.T) {
	isolate(t)
	s, _ := Load()
	p, _ := s.Add("Temp", "http://x", "m", true, 0)
	if err := s.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.All(); len(got) != 0 {
		t.Errorf("expected no entries after delete, got %d", len(got))
	}

	reloaded, _ := Load()
	if got := reloaded.All(); len(got) != 0 {
		t.Errorf("delete should persist — after reload got %d entries", len(got))
	}
}

func TestDeleteUnknownIDIsNotAnError(t *testing.T) {
	isolate(t)
	s, _ := Load()
	if err := s.Delete("does-not-exist"); err != nil {
		t.Errorf("deleting an unknown ID should be a no-op, got error: %v", err)
	}
}

func TestAllReturnsACopy(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_, _ = s.Add("A", "http://a", "m", true, 0)

	all := s.All()
	all[0].Name = "tampered"

	if got := s.All()[0].Name; got != "A" {
		t.Errorf("mutating the slice from All() should not affect the store, got %q", got)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := isolate(t)
	s, _ := Load()
	if _, err := s.Add("A", "http://a", "m", true, 0); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "kram-gateway", "custom_providers.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("custom providers file should exist at %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file permissions = %o, want 600", perm)
	}
}
