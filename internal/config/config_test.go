package config

import (
	"path/filepath"
	"testing"
)

func testConfig() *Config {
	return &Config{
		Providers: []ProviderConfig{
			{ID: "p1", Kind: "anthropic", APIKeyEnv: "ANTHROPIC_API_KEY", Model: "claude-sonnet-4-5"},
		},
		Combos: []ComboConfig{
			{ID: "default", Strategy: "smart", Providers: []string{"p1"},
				Response: ResponseGateConfig{RejectEmpty: true, RequireTerminal: true}},
		},
		DefaultCombo: "default",
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	want := testConfig()

	if err := Save(want, path); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 || got.Providers[0].ID != "p1" || got.Providers[0].Kind != "anthropic" {
		t.Errorf("providers did not round-trip: %+v", got.Providers)
	}
	if len(got.Combos) != 1 || got.Combos[0].Strategy != "smart" {
		t.Errorf("combos did not round-trip: %+v", got.Combos)
	}
	if !got.Combos[0].Response.RejectEmpty || !got.Combos[0].Response.RequireTerminal {
		t.Errorf("response gate config did not round-trip: %+v", got.Combos[0].Response)
	}
	if got.DefaultCombo != "default" {
		t.Errorf("DefaultCombo = %q, want %q", got.DefaultCombo, "default")
	}
}

func TestSaveOverwritesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	first := testConfig()
	if err := Save(first, path); err != nil {
		t.Fatal(err)
	}

	second := testConfig()
	second.Combos[0].Strategy = "round-robin"
	if err := Save(second, path); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Combos[0].Strategy != "round-robin" {
		t.Errorf("Save should overwrite an existing file, got strategy %q", got.Combos[0].Strategy)
	}
}

func TestSaveCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "config.yaml")
	if err := Save(testConfig(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after Save into a nonexistent nested dir: %v", err)
	}
}
