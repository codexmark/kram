package lsp

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/codexmark/kram-gateway/internal/kramhome"
)

// langSpec is what's needed to start and talk to one language's server:
// the process to launch, and the languageId to report in didOpen.
type langSpec struct {
	Key        string // cache/config key, e.g. "go", "typescript", "python"
	LanguageID string // LSP languageId reported for files of this language
	Command    string
	Args       []string
}

// builtinExtensions maps a file extension to the language it belongs to
// and that language's default server command — gopls for Go,
// typescript-language-server for the whole JS/TS family (one server
// process handles all of .ts/.tsx/.js/.jsx), pyright-langserver for
// Python.
var builtinExtensions = map[string]langSpec{
	".go":  {Key: "go", LanguageID: "go", Command: "gopls", Args: []string{}},
	".ts":  {Key: "typescript", LanguageID: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}},
	".tsx": {Key: "typescript", LanguageID: "typescriptreact", Command: "typescript-language-server", Args: []string{"--stdio"}},
	".js":  {Key: "typescript", LanguageID: "javascript", Command: "typescript-language-server", Args: []string{"--stdio"}},
	".jsx": {Key: "typescript", LanguageID: "javascriptreact", Command: "typescript-language-server", Args: []string{"--stdio"}},
	".py":  {Key: "python", LanguageID: "python", Command: "pyright-langserver", Args: []string{"--stdio"}},
}

// configEntry is one language's override in .kram/lsp.json — a command
// (and args) to use instead of the built-in default, and optionally a set
// of file extensions this entry owns (needed to add a language Kram has
// no built-in mapping for; not needed to just override go/typescript/
// python's command, since those extensions are already known).
type configEntry struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	LanguageID string   `json:"languageId,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
}

type lspConfigFile struct {
	Servers map[string]configEntry `json:"servers"`
}

// loadConfig merges the global override file (kramhome/lsp.json) with the
// project's own (<workspace>/.kram/lsp.json) — same two-tier, project-wins
// convention internal/mcp.LoadConfig and .kram/tools/*.json use. Same
// best-effort philosophy too: a missing or malformed file contributes
// nothing rather than failing anything, since most projects have no
// lsp.json at all.
func loadConfig(workspace string) map[string]configEntry {
	out := make(map[string]configEntry)
	if global, err := kramhome.Path("lsp.json"); err == nil {
		mergeConfigFile(out, global)
	}
	mergeConfigFile(out, filepath.Join(workspace, ".kram", "lsp.json"))
	return out
}

func mergeConfigFile(into map[string]configEntry, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cf lspConfigFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return
	}
	for key, entry := range cf.Servers {
		into[key] = entry
	}
}

// buildExtensionTable produces the effective extension -> langSpec map for
// one workspace: built-ins, with config overrides applied on top. A
// config entry keyed by an existing built-in language key (go/typescript/
// python) replaces that language's command/args but keeps its known
// extensions unless the entry also lists its own. A config entry keyed by
// a new name registers a new language entirely, provided it lists at
// least one extension and a command — an entry that doesn't is skipped
// rather than silently doing nothing useful.
func buildExtensionTable(workspace string) map[string]langSpec {
	table := make(map[string]langSpec, len(builtinExtensions))
	for ext, spec := range builtinExtensions {
		table[ext] = spec
	}

	cfg := loadConfig(workspace)
	// Apply command/args overrides for existing keys first.
	for ext, spec := range table {
		if entry, ok := cfg[spec.Key]; ok && entry.Command != "" {
			spec.Command = entry.Command
			spec.Args = entry.Args
			if entry.LanguageID != "" {
				spec.LanguageID = entry.LanguageID
			}
			table[ext] = spec
		}
	}
	// Register brand-new languages and/or additional extensions for known
	// ones.
	for key, entry := range cfg {
		if entry.Command == "" || len(entry.Extensions) == 0 {
			continue
		}
		languageID := entry.LanguageID
		if languageID == "" {
			languageID = key
		}
		for _, ext := range entry.Extensions {
			ext = normalizeExt(ext)
			if ext == "" {
				continue
			}
			table[ext] = langSpec{Key: key, LanguageID: languageID, Command: entry.Command, Args: entry.Args}
		}
	}
	return table
}

func normalizeExt(ext string) string {
	ext = strings.TrimSpace(ext)
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return strings.ToLower(ext)
}

// pathToURI converts an absolute filesystem path to a file:// URI.
func pathToURI(absPath string) string {
	slashed := filepath.ToSlash(absPath)
	if runtime.GOOS == "windows" && !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	u := url.URL{Scheme: "file", Path: slashed}
	return u.String()
}

// uriToPath converts a file:// URI back to a filesystem path, the inverse
// of pathToURI. Locations a server returns (e.g. a definition in a
// different file than the one queried) come back as URIs; tool output
// converts them to workspace-relative paths where possible so the model
// never has to reason about file:// syntax.
func uriToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" {
		return "", &unsupportedURIError{uri}
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) > 1 && p[0] == '/' {
		p = p[1:]
	}
	return filepath.FromSlash(p), nil
}

type unsupportedURIError struct{ uri string }

func (e *unsupportedURIError) Error() string { return "unsupported location URI: " + e.uri }
