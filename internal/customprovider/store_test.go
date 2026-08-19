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
	p, err := s.Add("Meu Servidor", "http://192.168.1.50:8080/v1", "llama-3")
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

func TestAddRequiresNameAndBaseURL(t *testing.T) {
	isolate(t)
	s, _ := Load()
	if _, err := s.Add("", "http://x", ""); err == nil {
		t.Error("expected an error for an empty name")
	}
	if _, err := s.Add("x", "", ""); err == nil {
		t.Error("expected an error for an empty base URL")
	}
}

func TestAddModelIsOptional(t *testing.T) {
	isolate(t)
	s, _ := Load()
	p, err := s.Add("Local", "http://localhost:9099/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "" {
		t.Errorf("Model = %q, want empty (passthrough)", p.Model)
	}
}

func TestDuplicateNamesGetDedupedIDs(t *testing.T) {
	isolate(t)
	s, _ := Load()
	a, err := s.Add("Servidor", "http://a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Add("Servidor", "http://b", "")
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
	p, err := s.Add("LM Studio (casa) — GPU #1!", "http://x", "")
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
	p, err := s.Add("!!!", "http://x", "")
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
	p, _ := s.Add("Temp", "http://x", "")
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
	_, _ = s.Add("A", "http://a", "")

	all := s.All()
	all[0].Name = "tampered"

	if got := s.All()[0].Name; got != "A" {
		t.Errorf("mutating the slice from All() should not affect the store, got %q", got)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := isolate(t)
	s, _ := Load()
	if _, err := s.Add("A", "http://a", ""); err != nil {
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
