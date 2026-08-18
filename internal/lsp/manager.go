package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// initializeTimeout bounds starting a language server process and running
// its initialize handshake. Longer than callTimeout because some real
// servers (gopls indexing a large module cache on first start, in
// particular) can be slow the very first time.
const initializeTimeout = 20 * time.Second

// diagnosticsWaitTimeout bounds how long lsp_diagnostics waits for a
// fresh textDocument/publishDiagnostics notification after (re)opening a
// file, before falling back to whatever's cached.
const diagnosticsWaitTimeout = 5 * time.Second

// clientEntry is a language server connection that may still be starting.
// ready is closed exactly once, after which client/err are safe to read
// without the entry's owning lock — this is what makes ClientFor start a
// given language's server exactly once even under concurrent tool calls.
type clientEntry struct {
	ready  chan struct{}
	client *Client
	err    error
}

// Manager owns every language server connection for one workspace, one
// per language, started lazily on first use. Nothing here touches a
// filesystem or spawns a process at construction time — the extension
// table itself is built lazily on first ClientFor call — so building a
// Manager at daemon startup (as tools.NewRegistry does) costs nothing and
// starts nothing, satisfying the "no server starts before it's actually
// asked for" invariant.
type Manager struct {
	workspace string

	tableOnce sync.Once
	extTable  map[string]langSpec

	mu      sync.Mutex
	clients map[string]*clientEntry

	// dial is overridable in tests to substitute a fake in-process server
	// (net.Pipe) for a real subprocess — see manager_test.go.
	dial func(spec langSpec, workspace string) (stream, error)
}

// NewManager builds a manager scoped to workspace. Cheap and side-effect
// free: no config file is read and no process is started until the first
// ClientFor call.
func NewManager(workspace string) *Manager {
	return &Manager{
		workspace: workspace,
		clients:   make(map[string]*clientEntry),
		dial: func(spec langSpec, workspace string) (stream, error) {
			return startProcess(spec.Command, spec.Args, workspace)
		},
	}
}

func (m *Manager) extensionTable() map[string]langSpec {
	m.tableOnce.Do(func() {
		m.extTable = buildExtensionTable(m.workspace)
	})
	return m.extTable
}

func (m *Manager) specForFile(relPath string) (langSpec, bool) {
	ext := strings.ToLower(filepath.Ext(relPath))
	spec, ok := m.extensionTable()[ext]
	return spec, ok
}

// ClientFor returns the language server connection that owns relPath's
// extension, starting it if this is the first request for that language
// in this workspace. Concurrent callers requesting the same language
// before it's finished starting all wait on the same in-flight attempt
// rather than racing to start it twice.
//
// A missing extension mapping, a missing/unusable server binary, or a
// failed initialize handshake all come back as a plain error — this
// method never panics and never brings down the caller; it's the tool
// layer's job to turn that error into "LSP capability unavailable for
// language X: <reason>" text for the model, per Kram's rule that a
// third-party tool being broken costs only its own capability.
func (m *Manager) ClientFor(ctx context.Context, relPath string) (*Client, error) {
	spec, ok := m.specForFile(relPath)
	if !ok {
		return nil, fmt.Errorf("no LSP server configured for extension %q", filepath.Ext(relPath))
	}

	m.mu.Lock()
	entry, exists := m.clients[spec.Key]
	if !exists {
		entry = &clientEntry{ready: make(chan struct{})}
		m.clients[spec.Key] = entry
	}
	m.mu.Unlock()

	if !exists {
		entry.client, entry.err = m.start(ctx, spec)
		close(entry.ready)
		return entry.client, entry.err
	}

	select {
	case <-entry.ready:
		return entry.client, entry.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) start(ctx context.Context, spec langSpec) (*Client, error) {
	initCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()

	s, err := m.dial(spec, m.workspace)
	if err != nil {
		return nil, fmt.Errorf("starting %s server (%s): %w", spec.Key, spec.Command, err)
	}
	client := newClient(spec.Key, s)
	if err := client.initialize(initCtx, m.workspace); err != nil {
		client.Close()
		return nil, fmt.Errorf("initializing %s server (%s): %w", spec.Key, spec.Command, err)
	}
	return client, nil
}

// openFile reads relPath's current on-disk content and (re)opens it in
// the given client — the "reopen on every query, no watcher, no
// incremental sync" policy documented on Client.ensureOpen. Returns the
// file:// URI the server now knows this content under.
func (m *Manager) openFile(ctx context.Context, client *Client, relPath string) (string, error) {
	absPath := filepath.Join(m.workspace, relPath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", relPath, err)
	}
	spec, _ := m.specForFile(relPath)
	uri := pathToURI(absPath)
	if err := client.ensureOpen(ctx, uri, spec.LanguageID, string(data)); err != nil {
		return "", fmt.Errorf("opening %s in %s server: %w", relPath, spec.Key, err)
	}
	return uri, nil
}

// Diagnostics returns whatever the language server has published for
// relPath after (re)opening it fresh.
func (m *Manager) Diagnostics(ctx context.Context, relPath string) ([]Diagnostic, error) {
	client, err := m.ClientFor(ctx, relPath)
	if err != nil {
		return nil, err
	}
	uri, err := m.openFile(ctx, client, relPath)
	if err != nil {
		return nil, err
	}
	return client.waitDiagnostics(ctx, uri, diagnosticsWaitTimeout), nil
}

// Definition resolves the symbol at a 0-indexed line/character in relPath.
func (m *Manager) Definition(ctx context.Context, relPath string, line, character int) ([]Location, error) {
	client, err := m.ClientFor(ctx, relPath)
	if err != nil {
		return nil, err
	}
	uri, err := m.openFile(ctx, client, relPath)
	if err != nil {
		return nil, err
	}
	return client.definition(ctx, uri, Position{Line: line, Character: character})
}

// References finds every reference to the symbol at a 0-indexed
// line/character in relPath, including the declaration itself.
func (m *Manager) References(ctx context.Context, relPath string, line, character int) ([]Location, error) {
	client, err := m.ClientFor(ctx, relPath)
	if err != nil {
		return nil, err
	}
	uri, err := m.openFile(ctx, client, relPath)
	if err != nil {
		return nil, err
	}
	return client.references(ctx, uri, Position{Line: line, Character: character}, true)
}

// DisplayPath converts a Location's URI to a path suitable for showing
// the model: workspace-relative when the location is inside the
// workspace (the common case), an absolute path when it isn't (e.g. a
// definition resolving into GOROOT or node_modules), and the raw URI as a
// last resort if it isn't even a file:// URI.
func (m *Manager) DisplayPath(uri string) string {
	p, err := uriToPath(uri)
	if err != nil {
		return uri
	}
	if rel, err := filepath.Rel(m.workspace, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// Close shuts down every language server this manager started (including
// any still mid-startup — Close waits for those to finish before closing
// them, so nothing is left running as an orphan after the daemon exits).
func (m *Manager) Close() {
	m.mu.Lock()
	entries := make([]*clientEntry, 0, len(m.clients))
	for _, e := range m.clients {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range entries {
		e := e
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-e.ready
			if e.client != nil {
				e.client.Close()
			}
		}()
	}
	wg.Wait()
}
