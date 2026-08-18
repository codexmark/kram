package tools

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/lsp"
)

// fakeLSPProvider is a scripted stand-in for *lsp.Manager, letting these
// tests check the tools' argument handling, formatting, and
// unavailability messaging without starting any real or fake language
// server subprocess — that end-to-end wire behavior is already covered
// by internal/lsp's own test suite (net.Pipe fake server).
type fakeLSPProvider struct {
	diagnostics    []lsp.Diagnostic
	diagnosticsErr error
	definitions    []lsp.Location
	definitionErr  error
	references     []lsp.Location
	referencesErr  error
	displayPaths   map[string]string // uri -> display path override; defaults to the uri itself

	lastRelPath string
	lastLine    int
	lastChar    int
}

func (f *fakeLSPProvider) Diagnostics(ctx context.Context, relPath string) ([]lsp.Diagnostic, error) {
	f.lastRelPath = relPath
	return f.diagnostics, f.diagnosticsErr
}

func (f *fakeLSPProvider) Definition(ctx context.Context, relPath string, line, character int) ([]lsp.Location, error) {
	f.lastRelPath, f.lastLine, f.lastChar = relPath, line, character
	return f.definitions, f.definitionErr
}

func (f *fakeLSPProvider) References(ctx context.Context, relPath string, line, character int) ([]lsp.Location, error) {
	f.lastRelPath, f.lastLine, f.lastChar = relPath, line, character
	return f.references, f.referencesErr
}

func (f *fakeLSPProvider) DisplayPath(uri string) string {
	if p, ok := f.displayPaths[uri]; ok {
		return p
	}
	return uri
}

func TestLSPDiagnostics_Formatting(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeLSPProvider{diagnostics: []lsp.Diagnostic{
		{Range: lsp.Range{Start: lsp.Position{Line: 10, Character: 2}}, Severity: lsp.SeverityError, Message: "undefined: foo"},
		{Range: lsp.Range{Start: lsp.Position{Line: 3, Character: 0}}, Severity: lsp.SeverityWarning, Message: "unused import"},
	}}
	tool := newLSPDiagnostics(dir, fake)

	out, err := tool.Execute(context.Background(), mustJSON(t, lspFileArgs{File: "main.go"}))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if fake.lastRelPath != "main.go" {
		t.Fatalf("expected relPath 'main.go', got %q", fake.lastRelPath)
	}

	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), out)
	}
	// Sorted by line ascending: line 3 warning before line 10 error.
	if !strings.HasPrefix(lines[0], "main.go:3:0 [warning] unused import") {
		t.Errorf("unexpected first line: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "main.go:10:2 [error] undefined: foo") {
		t.Errorf("unexpected second line: %q", lines[1])
	}
}

func TestLSPDiagnostics_NoneFound(t *testing.T) {
	dir := t.TempDir()
	tool := newLSPDiagnostics(dir, &fakeLSPProvider{})
	out, err := tool.Execute(context.Background(), mustJSON(t, lspFileArgs{File: "clean.go"}))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if !strings.Contains(out, "no diagnostics") {
		t.Errorf("expected a clean-file message, got %q", out)
	}
}

func TestLSPDiagnostics_ServerUnavailable(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeLSPProvider{diagnosticsErr: errors.New(`exec: "gopls": executable file not found in $PATH`)}
	tool := newLSPDiagnostics(dir, fake)

	out, err := tool.Execute(context.Background(), mustJSON(t, lspFileArgs{File: "main.go"}))
	if err != nil {
		t.Fatalf("Execute must never return a Go error for an unavailable server: %v", err)
	}
	if !strings.Contains(out, "LSP capability unavailable") {
		t.Errorf("expected an unavailability message, got %q", out)
	}
	if !strings.Contains(out, "go") {
		t.Errorf("expected the language to be named in the message, got %q", out)
	}
}

func TestLSPDiagnostics_PathEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	tool := newLSPDiagnostics(dir, &fakeLSPProvider{})
	out, err := tool.Execute(context.Background(), mustJSON(t, lspFileArgs{File: "../outside.go"}))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("expected an error result for a path escaping the workspace, got %q", out)
	}
}

func TestLSPDefinition_Formatting(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "other.go"), "package main\n\nfunc Helper() {}\n")

	fake := &fakeLSPProvider{
		definitions: []lsp.Location{
			{URI: "file:///workspace/other.go", Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}}},
		},
		displayPaths: map[string]string{"file:///workspace/other.go": "other.go"},
	}
	tool := newLSPDefinition(dir, fake)

	out, err := tool.Execute(context.Background(), mustJSON(t, lspPositionArgs{File: "main.go", Line: 9, Character: 4}))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if fake.lastLine != 9 || fake.lastChar != 4 {
		t.Fatalf("expected the query position to be forwarded unchanged, got line=%d char=%d", fake.lastLine, fake.lastChar)
	}
	if !strings.Contains(out, "other.go:2") {
		t.Errorf("expected the definition location in the output, got %q", out)
	}
	if !strings.Contains(out, "func Helper()") {
		t.Errorf("expected a source snippet for context, got %q", out)
	}
}

func TestLSPDefinition_NoneFound(t *testing.T) {
	dir := t.TempDir()
	tool := newLSPDefinition(dir, &fakeLSPProvider{})
	out, err := tool.Execute(context.Background(), mustJSON(t, lspPositionArgs{File: "main.go", Line: 0, Character: 0}))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if !strings.Contains(out, "no definition") {
		t.Errorf("expected a not-found message, got %q", out)
	}
}

func TestLSPReferences_FormattingAndSort(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeLSPProvider{
		references: []lsp.Location{
			{URI: "b", Range: lsp.Range{Start: lsp.Position{Line: 20, Character: 0}}},
			{URI: "a", Range: lsp.Range{Start: lsp.Position{Line: 5, Character: 0}}},
			{URI: "a", Range: lsp.Range{Start: lsp.Position{Line: 1, Character: 0}}},
		},
	}
	tool := newLSPReferences(dir, fake)

	out, err := tool.Execute(context.Background(), mustJSON(t, lspPositionArgs{File: "main.go", Line: 1, Character: 0}))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}

	want := "a:1\na:5\nb:20"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestLSPReferences_ServerUnavailable(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeLSPProvider{referencesErr: errors.New("timed out after 30s")}
	tool := newLSPReferences(dir, fake)

	out, err := tool.Execute(context.Background(), mustJSON(t, lspPositionArgs{File: "main.py", Line: 0, Character: 0}))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if !strings.Contains(out, "LSP capability unavailable for py") {
		t.Errorf("expected an unavailability message naming the language, got %q", out)
	}
}
