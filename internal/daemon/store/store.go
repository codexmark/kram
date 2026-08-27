// Package store persists sessions and messages to a local SQLite database
// so the daemon remains the single source of truth across restarts — a
// client disconnecting, or the daemon itself being restarted, never loses
// a session's history, including its tool-call trail.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the daemon a static, cgo-free binary

	"github.com/codexmark/kram/internal/openai"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id   TEXT NOT NULL REFERENCES sessions(id),
	role         TEXT NOT NULL,
	content      TEXT NOT NULL,
	provider     TEXT NOT NULL DEFAULT '',
	tool_calls   TEXT NOT NULL DEFAULT '',
	tool_call_id TEXT NOT NULL DEFAULT '',
	name         TEXT NOT NULL DEFAULT '',
	images       TEXT NOT NULL DEFAULT '',
	provider_items TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
`

// memorySchema is applied separately from schema — see memory.go — kept
// apart because it's a distinct concern (cross-session memory, not
// session/message persistence) with its own FTS5 virtual table and sync
// triggers.
const memorySchema = `
CREATE TABLE IF NOT EXISTS memory_entries (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	scope      TEXT NOT NULL,
	content    TEXT NOT NULL,
	pinned     INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory_entries(scope);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
	content, content='memory_entries', content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS memory_entries_ai AFTER INSERT ON memory_entries BEGIN
	INSERT INTO memory_fts(rowid, content) VALUES (new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS memory_entries_ad AFTER DELETE ON memory_entries BEGIN
	INSERT INTO memory_fts(memory_fts, rowid, content) VALUES('delete', old.id, old.content);
END;

CREATE TRIGGER IF NOT EXISTS memory_entries_au AFTER UPDATE ON memory_entries BEGIN
	INSERT INTO memory_fts(memory_fts, rowid, content) VALUES('delete', old.id, old.content);
	INSERT INTO memory_fts(rowid, content) VALUES (new.id, new.content);
END;
`

// Session is a durable conversation thread owned by the daemon.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Message is one turn within a session — a user message, an assistant
// reply (Provider set to whoever served it, ToolCalls set if it's
// requesting tool invocations instead of/before answering), or a tool
// result (Role "tool", ToolCallID + Name identifying which call it
// answers). Name doubles as the marker for a compaction summary message
// (Name == CompactionMarker) — see internal/daemon/compaction.
type Message struct {
	ID            int64                 `json:"id"`
	SessionID     string                `json:"session_id"`
	Role          string                `json:"role"`
	Content       string                `json:"content"`
	Provider      string                `json:"provider,omitempty"`
	ToolCalls     []openai.ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID    string                `json:"tool_call_id,omitempty"`
	Name          string                `json:"name,omitempty"`
	Images        []string              `json:"images,omitempty"`
	ProviderItems []openai.ProviderItem `json:"provider_items,omitempty"`
	CreatedAt     int64                 `json:"created_at"`
}

// Store wraps the SQLite connection and the daemon's persistence methods.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// the schema. The connection pool is capped at 1 writer to avoid SQLite's
// "database is locked" errors under concurrent writes — fine for a local
// daemon's request volume.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	// WAL lets a reader and the single writer proceed without blocking
	// each other, and survives a crash mid-transaction cleanly; busy_timeout
	// makes a second kram instance in the same workspace wait for the lock
	// briefly instead of failing outright with SQLITE_BUSY (an easy thing
	// to hit — two terminals in one project). Applied before the schema so
	// every write below already runs under WAL. A PRAGMA that errors here
	// means the durability guarantees this store is built on aren't in
	// effect, so fail loudly rather than run degraded.
	for _, pragma := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("applying %q: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	if err := ensureProviderItemsColumn(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(memorySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying memory schema: %w", err)
	}
	if _, err := db.Exec(searchSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying search schema: %w", err)
	}
	if err := backfillMessagesFTS(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("backfilling message search index: %w", err)
	}

	return &Store{db: db}, nil
}

func ensureProviderItemsColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return fmt.Errorf("inspecting messages schema: %w", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == "provider_items" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN provider_items TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("adding provider_items column: %w", err)
	}
	return nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateSession inserts a new, empty session.
func (s *Store) CreateSession(id, title string) (Session, error) {
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, title, now, now,
	)
	if err != nil {
		return Session{}, fmt.Errorf("creating session: %w", err)
	}
	return Session{ID: id, Title: title, CreatedAt: now, UpdatedAt: now}, nil
}

// ListSessions returns all sessions, most recently updated first.
func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, title, created_at, updated_at FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning session row: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// GetSession returns one session and returns sql.ErrNoRows if it doesn't exist.
func (s *Store) GetSession(id string) (Session, error) {
	var sess Session
	err := s.db.QueryRow(`SELECT id, title, created_at, updated_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Title, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// ListMessages returns every message in a session, oldest first, including
// the full tool-call/tool-result trail and any compaction summaries.
func (s *Store) ListMessages(sessionID string) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, role, content, provider, tool_calls, tool_call_id, name, images, provider_items, created_at
		 FROM messages WHERE session_id = ? ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMessage(row rowScanner) (Message, error) {
	var m Message
	var toolCallsJSON, imagesJSON, providerItemsJSON string
	if err := row.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Provider, &toolCallsJSON, &m.ToolCallID, &m.Name, &imagesJSON, &providerItemsJSON, &m.CreatedAt); err != nil {
		return Message{}, fmt.Errorf("scanning message row: %w", err)
	}
	if err := decodeMessageJSON(&m, toolCallsJSON, imagesJSON, providerItemsJSON); err != nil {
		return Message{}, err
	}
	return m, nil
}

// decodeMessageJSON fills in a Message's ToolCalls/Images from their
// stored JSON-text columns — split out of scanMessage so search.go's
// SearchMessages, which scans one extra (session title) column that
// doesn't fit scanMessage's fixed 9-column shape, can reuse the same
// decoding instead of duplicating it.
func decodeMessageJSON(m *Message, toolCallsJSON, imagesJSON string, providerItemsJSON ...string) error {
	if toolCallsJSON != "" {
		if err := json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls); err != nil {
			return fmt.Errorf("decoding stored tool_calls: %w", err)
		}
	}
	if imagesJSON != "" {
		if err := json.Unmarshal([]byte(imagesJSON), &m.Images); err != nil {
			return fmt.Errorf("decoding stored images: %w", err)
		}
	}
	if len(providerItemsJSON) > 0 && providerItemsJSON[0] != "" {
		if err := json.Unmarshal([]byte(providerItemsJSON[0]), &m.ProviderItems); err != nil {
			return fmt.Errorf("decoding stored provider_items: %w", err)
		}
	}
	return nil
}

// AppendMessage stores one message and touches the parent session's
// updated_at timestamp. Callers set only the fields relevant to the
// message's role; ID and CreatedAt are assigned here.
func (s *Store) AppendMessage(sessionID string, msg Message) (Message, error) {
	now := time.Now().Unix()

	var toolCallsJSON, imagesJSON, providerItemsJSON string
	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return Message{}, fmt.Errorf("encoding tool_calls: %w", err)
		}
		toolCallsJSON = string(b)
	}
	if len(msg.Images) > 0 {
		b, err := json.Marshal(msg.Images)
		if err != nil {
			return Message{}, fmt.Errorf("encoding images: %w", err)
		}
		imagesJSON = string(b)
	}
	if len(msg.ProviderItems) > 0 {
		b, err := json.Marshal(msg.ProviderItems)
		if err != nil {
			return Message{}, fmt.Errorf("encoding provider_items: %w", err)
		}
		providerItemsJSON = string(b)
	}

	// Insert the message and touch the session's updated_at in one
	// transaction, so a crash between the two can't leave a session whose
	// timestamp disagrees with its latest message — and, more importantly,
	// so the write is a single atomic unit rather than two separate
	// autocommits.
	tx, err := s.db.Begin()
	if err != nil {
		return Message{}, fmt.Errorf("beginning message transaction: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit; rolls back on any early return

	res, err := tx.Exec(
		`INSERT INTO messages (session_id, role, content, provider, tool_calls, tool_call_id, name, images, provider_items, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, msg.Role, msg.Content, msg.Provider, toolCallsJSON, msg.ToolCallID, msg.Name, imagesJSON, providerItemsJSON, now,
	)
	if err != nil {
		return Message{}, fmt.Errorf("appending message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("reading inserted message id: %w", err)
	}

	if _, err := tx.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return Message{}, fmt.Errorf("touching session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Message{}, fmt.Errorf("committing message: %w", err)
	}

	msg.ID = id
	msg.SessionID = sessionID
	msg.CreatedAt = now
	return msg, nil
}
