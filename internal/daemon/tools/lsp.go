// Package tools: LSP tools give the model real semantic navigation
// (diagnostics, go-to-definition, find-references) on top of the
// grep/glob/read_file tools, which find text but don't understand
// structure. Backed by internal/lsp, which lazily starts one language
// server per language the first time it's needed and reuses that
// connection afterward.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codexmark/kram/internal/lsp"
)

// lspProvider is the subset of *lsp.Manager the LSP tools need. Defined
// as an interface (rather than depending on *lsp.Manager directly) so
// unit tests can substitute a fake without starting any real or fake
// language server process — see lsp_test.go.
type lspProvider interface {
	Diagnostics(ctx context.Context, relPath string) ([]lsp.Diagnostic, error)
	Definition(ctx context.Context, relPath string, line, character int) ([]lsp.Location, error)
	References(ctx context.Context, relPath string, line, character int) ([]lsp.Location, error)
	DisplayPath(uri string) string
}

var _ lspProvider = (*lsp.Manager)(nil)

// positionDoc is appended to every LSP tool's description so the model
// only has to learn the indexing convention once, in one consistent
// place, rather than guessing per tool.
const positionDoc = " Line and character positions are 0-indexed, the native LSP convention: the file's first line is line 0, and the first column on a line is character 0."

// workspaceRelPath validates userPath is inside workspace (reusing the
// same boundary every file/shell tool enforces) and returns it as a path
// relative to workspace root, which is the form internal/lsp.Manager's
// API takes.
func workspaceRelPath(workspace, userPath string) (string, error) {
	abs, err := resolvePath(workspace, userPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(workspace, abs)
	if err != nil {
		return "", err
	}
	return rel, nil
}

// languageLabel gives a short, human-readable name for a file's LSP
// unavailability message ("go", "py", "this file" when there's no
// extension to go on) without duplicating internal/lsp's full
// extension-to-server table here.
func languageLabel(relPath string) string {
	ext := strings.TrimPrefix(filepath.Ext(relPath), ".")
	if ext == "" {
		return "this file"
	}
	return ext
}

// unavailable formats the one message every LSP tool returns instead of a
// Go error when the language server for a file's language can't be
// reached — missing binary, failed handshake, unrecognized extension,
// whatever. Never fatal: the model sees this text and can fall back to
// grep, same principle as an unreachable MCP server costing only its own
// tools.
func unavailable(relPath string, err error) string {
	return fmt.Sprintf("LSP capability unavailable for %s: %v", languageLabel(relPath), err)
}

// --- lsp_diagnostics ---

type lspDiagnostics struct {
	workspace string
	provider  lspProvider
}

func newLSPDiagnostics(workspace string, p lspProvider) *lspDiagnostics {
	return &lspDiagnostics{workspace: workspace, provider: p}
}

func (t *lspDiagnostics) Name() string { return "lsp_diagnostics" }
func (t *lspDiagnostics) Description() string {
	return "Get language-server diagnostics (compile errors, type errors, lint warnings) for one file. " +
		"Starts the language server for that file's language on first use in this workspace (gopls for Go, " +
		"typescript-language-server for JS/TS, pyright-langserver for Python) — the first call for a given " +
		"language may take a few seconds, later calls reuse the running server. Requires the corresponding " +
		"server binary to be installed; if it isn't, this reports that plainly instead of failing." + positionDoc
}

func (t *lspDiagnostics) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file": {"type": "string", "description": "File path relative to the project root."}
		},
		"required": ["file"]
	}`)
}

type lspFileArgs struct {
	File string `json:"file"`
}

func (t *lspDiagnostics) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args lspFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	relPath, err := workspaceRelPath(t.workspace, args.File)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	diags, err := t.provider.Diagnostics(ctx, relPath)
	if err != nil {
		return unavailable(relPath, err), nil
	}
	if len(diags) == 0 {
		return fmt.Sprintf("(no diagnostics for %s)", relPath), nil
	}

	sort.Slice(diags, func(i, j int) bool {
		if diags[i].Range.Start.Line != diags[j].Range.Start.Line {
			return diags[i].Range.Start.Line < diags[j].Range.Start.Line
		}
		return diags[i].Range.Start.Character < diags[j].Range.Start.Character
	})

	var b strings.Builder
	for _, d := range diags {
		fmt.Fprintf(&b, "%s:%d:%d [%s] %s\n", relPath, d.Range.Start.Line, d.Range.Start.Character, d.SeverityLabel(), d.Message)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// --- lsp_definition ---

type lspDefinition struct {
	workspace string
	provider  lspProvider
}

func newLSPDefinition(workspace string, p lspProvider) *lspDefinition {
	return &lspDefinition{workspace: workspace, provider: p}
}

func (t *lspDefinition) Name() string { return "lsp_definition" }
func (t *lspDefinition) Description() string {
	return "Find where the symbol at a file position is defined (go-to-definition). Give the file and the " +
		"position of a use of the symbol (a function call, an identifier, a type reference). Returns each " +
		"definition's file and line, plus a short source snippet for context. Same lazy per-language server " +
		"startup as lsp_diagnostics." + positionDoc
}

func (t *lspDefinition) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file": {"type": "string", "description": "File path relative to the project root."},
			"line": {"type": "integer", "description": "0-indexed line of the symbol use."},
			"character": {"type": "integer", "description": "0-indexed character (column) of the symbol use on that line."}
		},
		"required": ["file", "line", "character"]
	}`)
}

type lspPositionArgs struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

func (t *lspDefinition) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args lspPositionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	relPath, err := workspaceRelPath(t.workspace, args.File)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	locs, err := t.provider.Definition(ctx, relPath, args.Line, args.Character)
	if err != nil {
		return unavailable(relPath, err), nil
	}
	if len(locs) == 0 {
		return "(no definition found)", nil
	}

	var b strings.Builder
	for i, loc := range locs {
		if i > 0 {
			b.WriteString("\n")
		}
		path := t.provider.DisplayPath(loc.URI)
		fmt.Fprintf(&b, "%s:%d", path, loc.Range.Start.Line)
		if snippet := sourceSnippet(t.workspace, path, loc.Range.Start.Line); snippet != "" {
			fmt.Fprintf(&b, "\n    %s", snippet)
		}
	}
	return b.String(), nil
}

// sourceSnippet returns the trimmed source line at a 0-indexed line
// number, for definition/reference output — best-effort: an unreadable
// file (e.g. deleted since the server indexed it, or a location the
// server reported oddly) just means no snippet, not a tool failure.
func sourceSnippet(workspace, displayPath string, line int) string {
	abs := displayPath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workspace, displayPath)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if line < 0 || line >= len(lines) {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(lines[line], "\r"))
}

// --- lsp_references ---

type lspReferences struct {
	workspace string
	provider  lspProvider
}

func newLSPReferences(workspace string, p lspProvider) *lspReferences {
	return &lspReferences{workspace: workspace, provider: p}
}

func (t *lspReferences) Name() string { return "lsp_references" }
func (t *lspReferences) Description() string {
	return "Find every reference to the symbol at a file position (find-references), including its " +
		"declaration. Give the file and the position of a use or the declaration itself. Returns each " +
		"reference's file and line. Same lazy per-language server startup as lsp_diagnostics." + positionDoc
}

func (t *lspReferences) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file": {"type": "string", "description": "File path relative to the project root."},
			"line": {"type": "integer", "description": "0-indexed line of the symbol."},
			"character": {"type": "integer", "description": "0-indexed character (column) of the symbol on that line."}
		},
		"required": ["file", "line", "character"]
	}`)
}

func (t *lspReferences) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args lspPositionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	relPath, err := workspaceRelPath(t.workspace, args.File)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	locs, err := t.provider.References(ctx, relPath, args.Line, args.Character)
	if err != nil {
		return unavailable(relPath, err), nil
	}
	if len(locs) == 0 {
		return "(no references found)", nil
	}

	type ref struct {
		path string
		line int
	}
	refs := make([]ref, len(locs))
	for i, loc := range locs {
		refs[i] = ref{path: t.provider.DisplayPath(loc.URI), line: loc.Range.Start.Line}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].path != refs[j].path {
			return refs[i].path < refs[j].path
		}
		return refs[i].line < refs[j].line
	})

	var b strings.Builder
	for i, r := range refs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s:%d", r.path, r.line)
	}
	return b.String(), nil
}
