// Package store persists sessions and messages to a local SQLite database
// so the daemon remains the single source of truth across restarts — a
// client disconnecting, or the daemon itself being restarted, never loses
// a session's history.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the daemon a static, cgo-free binary
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL REFERENCES sessions(id),
	role       TEXT NOT NULL,
	content    TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id);
`

// Session is a durable conversation thread owned by the daemon.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Message is one turn within a session.
type Message struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
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

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}

	return &Store{db: db}, nil
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

// ListMessages returns every message in a session, oldest first.
func (s *Store) ListMessages(sessionID string) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, role, content, created_at FROM messages WHERE session_id = ? ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning message row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AppendMessage stores one message and touches the parent session's
// updated_at timestamp.
func (s *Store) AppendMessage(sessionID, role, content string) (Message, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO messages (session_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		sessionID, role, content, now,
	)
	if err != nil {
		return Message{}, fmt.Errorf("appending message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("reading inserted message id: %w", err)
	}

	if _, err := s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return Message{}, fmt.Errorf("touching session: %w", err)
	}

	return Message{ID: id, SessionID: sessionID, Role: role, Content: content, CreatedAt: now}, nil
}
