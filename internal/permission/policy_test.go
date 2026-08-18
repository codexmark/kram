package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigMissingFilesIsEmptyNotError(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	pf := LoadConfig(workspace)
	if len(pf.Rules) != 0 || pf.Default != "" {
		t.Errorf("expected an empty policy with no files present, got %+v", pf)
	}
}

func TestLoadConfigMergesGlobalAndProject(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	workspace := t.TempDir()

	writeJSON(t, filepath.Join(xdg, "kram-gateway", "permissions.json"),
		`{"rules":[{"tool":"bash","pattern":"*","decision":"ask"}]}`)
	writeJSON(t, filepath.Join(workspace, ".kram", "permissions.json"),
		`{"rules":[{"tool":"bash","pattern":"go test *","decision":"allow"}]}`)

	pf := LoadConfig(workspace)
	if len(pf.Rules) != 2 {
		t.Fatalf("expected 2 merged rules, got %d: %+v", len(pf.Rules), pf.Rules)
	}

	e := NewEvaluator(pf, nil)
	if got := e.Evaluate("bash", "go test ./..."); got != Allow {
		t.Errorf("project rule should win by specificity: got %s, want allow", got)
	}
	if got := e.Evaluate("bash", "curl evil.com"); got != Ask {
		t.Errorf("global wildcard should still apply: got %s, want ask", got)
	}
}

func TestLoadConfigMalformedFileIsSkipped(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	writeJSON(t, filepath.Join(workspace, ".kram", "permissions.json"), `{not valid json`)

	pf := LoadConfig(workspace)
	if len(pf.Rules) != 0 {
		t.Errorf("a malformed policy file should contribute nothing, got %+v", pf.Rules)
	}
}

func TestLoadConfigDropsRulesWithInvalidDecisionOrEmptyTool(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	writeJSON(t, filepath.Join(workspace, ".kram", "permissions.json"), `{
		"rules": [
			{"tool": "bash", "decision": "maybe"},
			{"tool": "", "decision": "deny"},
			{"tool": "bash", "decision": "deny"}
		]
	}`)

	pf := LoadConfig(workspace)
	if len(pf.Rules) != 1 {
		t.Fatalf("expected only the one valid rule to survive, got %d: %+v", len(pf.Rules), pf.Rules)
	}
	if pf.Rules[0].Decision != Deny {
		t.Errorf("wrong rule survived: %+v", pf.Rules[0])
	}
}

func TestLoadConfigProjectDefaultOverridesGlobalDefault(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	workspace := t.TempDir()

	writeJSON(t, filepath.Join(xdg, "kram-gateway", "permissions.json"), `{"default":"allow"}`)
	writeJSON(t, filepath.Join(workspace, ".kram", "permissions.json"), `{"default":"ask"}`)

	pf := LoadConfig(workspace)
	if pf.Default != Ask {
		t.Errorf("project default should win, got %s", pf.Default)
	}
}
