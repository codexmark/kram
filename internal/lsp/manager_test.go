package lsp

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDial builds a Manager.dial replacement that hands out a fresh
// net.Pipe-backed fakeServer per language start, letting tests script
// each method's response the same way client_test.go does, but exercised
// through the Manager (extension detection, lazy start, file open,
// URI<->path conversion) rather than a *Client directly.
func fakeDial(t *testing.T, configure func(fs *fakeServer)) func(spec langSpec, workspace string) (stream, error) {
	return func(spec langSpec, workspace string) (stream, error) {
		clientConn, serverConn := net.Pipe()
		fs := &fakeServer{t: t, conn: serverConn, handlers: make(map[string]func(*fakeServer, envelope))}
		fs.handlers["initialize"] = func(fs *fakeServer, env envelope) {
			fs.respondResult(env.ID, initializeResult{Capabilities: json.RawMessage(`{}`)})
		}
		if configure != nil {
			configure(fs)
		}
		go fs.loop()
		t.Cleanup(func() { _ = serverConn.Close() })
		return clientConn, nil
	}
}

func TestManager_NewManagerStartsNothing(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	if len(m.clients) != 0 {
		t.Fatalf("NewManager must not pre-populate any client, got %d", len(m.clients))
	}
	// Building the manager must not even have read a config file yet —
	// the extension table is built lazily on first use.
	if m.extTable != nil {
		t.Fatal("extension table must not be built before the first ClientFor call")
	}
}

func TestManager_LazyStartAndReuse(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n")

	m := NewManager(dir)
	var dialCount int32
	m.dial = func(spec langSpec, workspace string) (stream, error) {
		atomic.AddInt32(&dialCount, 1)
		return fakeDial(t, nil)(spec, workspace)
	}

	if atomic.LoadInt32(&dialCount) != 0 {
		t.Fatal("no server should be dialed before the first ClientFor call")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := m.ClientFor(ctx, "main.go"); err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if got := atomic.LoadInt32(&dialCount); got != 1 {
		t.Fatalf("expected exactly 1 dial after the first ClientFor, got %d", got)
	}

	if _, err := m.ClientFor(ctx, "main.go"); err != nil {
		t.Fatalf("second ClientFor: %v", err)
	}
	if got := atomic.LoadInt32(&dialCount); got != 1 {
		t.Fatalf("expected the existing connection to be reused, got %d dials", got)
	}

	m.Close()
}

func TestManager_UnknownExtension(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	if _, err := m.ClientFor(context.Background(), "README.md"); err == nil {
		t.Fatal("expected an error for a file extension with no configured LSP server")
	}
}

func TestManager_MissingServerBinaryIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n")

	m := NewManager(dir)
	m.dial = func(spec langSpec, workspace string) (stream, error) {
		return startProcess("kram-lsp-definitely-does-not-exist-xyz", nil, workspace)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := m.ClientFor(ctx, "main.go")
	if err == nil {
		t.Fatal("expected an error for a missing server binary, not a panic or a fatal condition")
	}

	// A second call must return the same remembered failure promptly,
	// not attempt to spawn the missing binary all over again.
	start := time.Now()
	if _, err2 := m.ClientFor(ctx, "main.go"); err2 == nil {
		t.Fatal("expected the cached failure again on retry")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected the cached failure to return immediately, took %s", elapsed)
	}
}

func TestManager_DiagnosticsDefinitionReferences(t *testing.T) {
	dir := t.TempDir()
	mainPath := writeFile(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	otherPath := writeFile(t, dir, "other.go", "package main\n\nfunc Helper() {}\n")

	m := NewManager(dir)
	m.dial = fakeDial(t, func(fs *fakeServer) {
		fs.handlers["textDocument/didOpen"] = func(fs *fakeServer, env envelope) {
			var params didOpenParams
			_ = json.Unmarshal(env.Params, &params)
			fs.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{
				URI: params.TextDocument.URI,
				Diagnostics: []Diagnostic{
					{Range: Range{Start: Position{Line: 2, Character: 5}}, Severity: SeverityWarning, Message: "unused import"},
				},
			})
		}
		fs.handlers["textDocument/definition"] = func(fs *fakeServer, env envelope) {
			fs.respondResult(env.ID, Location{
				URI:   pathToURI(otherPath),
				Range: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 11}},
			})
		}
		fs.handlers["textDocument/references"] = func(fs *fakeServer, env envelope) {
			fs.respondResult(env.ID, []Location{
				{URI: pathToURI(mainPath), Range: Range{Start: Position{Line: 2, Character: 5}}},
				{URI: pathToURI(otherPath), Range: Range{Start: Position{Line: 2, Character: 5}}},
			})
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	diags, err := m.Diagnostics(ctx, "main.go")
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diags) != 1 || diags[0].Message != "unused import" {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}

	locs, err := m.Definition(ctx, "main.go", 2, 5)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 definition location, got %d", len(locs))
	}
	if got := m.DisplayPath(locs[0].URI); got != "other.go" {
		t.Fatalf("DisplayPath: got %q, want workspace-relative %q", got, "other.go")
	}

	refs, err := m.References(ctx, "main.go", 2, 5)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 reference locations, got %d", len(refs))
	}

	m.Close()
}

func TestManager_DisplayPathOutsideWorkspaceFallsBackToAbsolute(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	outside := filepath.Join(t.TempDir(), "vendor.go")
	got := m.DisplayPath(pathToURI(outside))
	if got != outside {
		t.Fatalf("expected the absolute path for a location outside the workspace, got %q", got)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}
