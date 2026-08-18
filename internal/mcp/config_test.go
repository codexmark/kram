package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestServerConfigKindDetection(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
		want serverKind
	}{
		{"explicit stdio", ServerConfig{Type: "stdio"}, kindStdio},
		{"explicit http", ServerConfig{Type: "http"}, kindHTTP},
		{"explicit sse treated as http", ServerConfig{Type: "sse"}, kindHTTP},
		{"inferred from command", ServerConfig{Command: "npx"}, kindStdio},
		{"inferred from url", ServerConfig{URL: "https://example.com/mcp"}, kindHTTP},
		{"neither set", ServerConfig{}, kindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.kind(); got != tc.want {
				t.Errorf("kind() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServerConfigEnabledDefaultsTrue(t *testing.T) {
	cfg := ServerConfig{Command: "x"}
	if !cfg.enabled() {
		t.Error("a server config with no Enabled field should default to enabled")
	}

	off := false
	cfg.Enabled = &off
	if cfg.enabled() {
		t.Error("Enabled: false should be respected")
	}

	on := true
	cfg.Enabled = &on
	if !cfg.enabled() {
		t.Error("Enabled: true should be respected")
	}
}

func TestLoadConfigMergesGlobalAndProject(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", globalDir)
	writeJSONFile(t, filepath.Join(globalDir, "kram-gateway", "mcp.json"), map[string]any{
		"mcpServers": map[string]any{
			"filesystem": map[string]any{"command": "mcp-filesystem"},
			"shared":     map[string]any{"command": "global-version"},
		},
	})

	workspace := t.TempDir()
	writeJSONFile(t, filepath.Join(workspace, ".kram", "mcp.json"), map[string]any{
		"mcpServers": map[string]any{
			"shared":  map[string]any{"command": "project-version"},
			"project": map[string]any{"command": "only-here"},
		},
	})

	cfg := LoadConfig(workspace)

	if len(cfg) != 3 {
		t.Fatalf("expected 3 merged servers (filesystem, shared, project), got %d: %+v", len(cfg), cfg)
	}
	if cfg["filesystem"].Command != "mcp-filesystem" {
		t.Errorf("filesystem should come from the global config, got %+v", cfg["filesystem"])
	}
	if cfg["shared"].Command != "project-version" {
		t.Errorf("a project entry should win over a global one with the same name, got %+v", cfg["shared"])
	}
	if cfg["project"].Command != "only-here" {
		t.Errorf("project should come from the project config, got %+v", cfg["project"])
	}
}

func TestLoadConfigMissingFilesIsNotAnError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := LoadConfig(t.TempDir())
	if len(cfg) != 0 {
		t.Errorf("expected an empty config when neither file exists, got %+v", cfg)
	}
}

func TestLoadConfigMalformedFileIsSkipped(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	kramDir := filepath.Join(workspace, ".kram")
	if err := os.MkdirAll(kramDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kramDir, "mcp.json"), []byte("not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig(workspace) // must not panic or error
	if len(cfg) != 0 {
		t.Errorf("a malformed config file should contribute nothing, got %+v", cfg)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
