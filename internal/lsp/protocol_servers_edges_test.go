package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticSeverityLabels(t *testing.T) {
	for _, tc := range []struct {
		severity int
		want     string
	}{{SeverityError, "error"}, {SeverityWarning, "warning"}, {SeverityInformation, "info"}, {SeverityHint, "hint"}, {0, "unknown"}, {99, "unknown"}} {
		if got := (Diagnostic{Severity: tc.severity}).SeverityLabel(); got != tc.want {
			t.Fatalf("severity %d=%q", tc.severity, got)
		}
	}
	if got := (&rpcError{Message: "broken"}).Error(); got != "broken" {
		t.Fatalf("rpc error=%q", got)
	}
}

func TestParseLocationsUnionAndErrors(t *testing.T) {
	r := Range{Start: Position{Line: 1, Character: 2}, End: Position{Line: 3, Character: 4}}
	cases := []struct {
		name      string
		raw       string
		wantURI   string
		wantRange Range
		wantLen   int
	}{
		{"empty", "  \n", "", Range{}, 0}, {"null", "\t null \r", "", Range{}, 0},
		{"single", `{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":3,"character":4}}}`, "file:///a.go", r, 1},
		{"link selection", `[{"targetUri":"file:///b.go","targetRange":{"start":{"line":8}},"targetSelectionRange":{"start":{"line":9}}}]`, "file:///b.go", Range{Start: Position{Line: 9}}, 1},
		{"link target", `[{"targetUri":"file:///c.go","targetRange":{"start":{"line":8}}}]`, "file:///c.go", Range{Start: Position{Line: 8}}, 1},
		{"missing ranges", `[{"targetUri":"file:///d.go"},{"uri":"file:///e.go"}]`, "file:///d.go", Range{}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLocations(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("locations=%#v", got)
			}
			if tc.wantLen > 0 && (got[0].URI != tc.wantURI || got[0].Range != tc.wantRange) {
				t.Fatalf("location=%#v", got[0])
			}
		})
	}
	for _, raw := range []string{"{", "[{"} {
		if _, err := parseLocations(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if got := string(trimSpaceBytes([]byte(" \t\nabc\r "))); got != "abc" {
		t.Fatalf("trim=%q", got)
	}
	if got := string(trimSpaceBytes(nil)); got != "" {
		t.Fatalf("nil trim=%q", got)
	}
}

func TestExtensionConfigOverridesAndNewLanguages(t *testing.T) {
	workspace := t.TempDir()
	dir := filepath.Join(workspace, ".kram")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"servers":{"go":{"command":"custom-gopls","args":["serve"],"languageId":"golang"},"rust":{"command":"rust-analyzer","extensions":[" rs ",".RUST",""]},"skip-command":{"extensions":["x"]},"skip-ext":{"command":"x"}}}`
	if err := os.WriteFile(filepath.Join(dir, "lsp.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	table := buildExtensionTable(workspace)
	if table[".go"].Command != "custom-gopls" || table[".go"].LanguageID != "golang" || table[".go"].Args[0] != "serve" {
		t.Fatalf("go=%#v", table[".go"])
	}
	if table[".rs"].Command != "rust-analyzer" || table[".rust"].LanguageID != "rust" {
		t.Fatalf("rust=%#v %#v", table[".rs"], table[".rust"])
	}
	if _, ok := table[".x"]; ok {
		t.Fatal("incomplete config registered")
	}
	if normalizeExt(" TSX ") != ".tsx" || normalizeExt(" .GO ") != ".go" || normalizeExt(" ") != "" {
		t.Fatal("normalization")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	into := map[string]configEntry{"keep": {Command: "yes"}}
	mergeConfigFile(into, bad)
	mergeConfigFile(into, bad+"-missing")
	if into["keep"].Command != "yes" {
		t.Fatal("best effort merge mutated map")
	}
}

func TestPathURIRoundTripAndUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "with space", "x.go")
	uri := pathToURI(path)
	if !strings.HasPrefix(uri, "file://") || !strings.Contains(uri, "with%20space") {
		t.Fatalf("uri=%q", uri)
	}
	got, err := uriToPath(uri)
	if err != nil || got != path {
		t.Fatalf("roundtrip=%q,%v want %q", got, err, path)
	}
	if _, err := uriToPath("https://example.com/x"); err == nil || !strings.Contains(err.Error(), "unsupported location URI") {
		t.Fatalf("err=%v", err)
	}
	if _, err := uriToPath("://bad"); err == nil {
		t.Fatal("invalid URI accepted")
	}
}
