package store

import (
	"fmt"
	"strings"
	"time"
)

// MemoryEntry is one compiled fact or note the agent chose to remember —
// written by the agent itself via a tool call, not captured automatically
// from raw conversation logs. Scope is either a workspace path (memory
// specific to one project) or GlobalScope (memory that applies everywhere).
type MemoryEntry struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Content   string `json:"content"`
	Pinned    bool   `json:"pinned"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// GlobalScope holds memory that applies across every project, not just
// the one it was written in — e.g. a standing user preference.
const GlobalScope = "_global"

// WriteMemoryEntry stores one compiled memory entry.
func (s *Store) WriteMemoryEntry(scope, content string, pinned bool) (MemoryEntry, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO memory_entries (scope, content, pinned, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		scope, content, boolToInt(pinned), now, now,
	)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("writing memory entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("reading inserted memory id: %w", err)
	}
	return MemoryEntry{ID: id, Scope: scope, Content: content, Pinned: pinned, CreatedAt: now, UpdatedAt: now}, nil
}

// RecentMemory returns pinned entries first, then the most recently
// updated ones, across the given scopes (typically the current workspace
// plus GlobalScope) — this is what gets injected automatically at the
// start of every turn, so it's kept small and bounded by limit; anything
// older or from another scope is reachable via SearchMemory instead.
func (s *Store) RecentMemory(scopes []string, limit int) ([]MemoryEntry, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	placeholders, args := scopePlaceholders(scopes)
	args = append(args, limit)

	rows, err := s.db.Query(
		`SELECT id, scope, content, pinned, created_at, updated_at FROM memory_entries
		 WHERE scope IN (`+placeholders+`)
		 ORDER BY pinned DESC, updated_at DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("listing recent memory: %w", err)
	}
	defer rows.Close()
	return scanMemoryRows(rows)
}

// SearchMemory does a full-text search (SQLite FTS5) over memory content
// within the given scopes — this is the agent's memory_search tool.
func (s *Store) SearchMemory(scopes []string, query string, limit int) ([]MemoryEntry, error) {
	if len(scopes) == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	placeholders, args := scopePlaceholders(scopes)
	args = append(args, query, limit)

	rows, err := s.db.Query(
		`SELECT m.id, m.scope, m.content, m.pinned, m.created_at, m.updated_at
		 FROM memory_fts f
		 JOIN memory_entries m ON m.id = f.rowid
		 WHERE m.scope IN (`+placeholders+`) AND f.content MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("searching memory: %w", err)
	}
	defer rows.Close()
	return scanMemoryRows(rows)
}

func scopePlaceholders(scopes []string) (string, []any) {
	placeholders := make([]string, len(scopes))
	args := make([]any, len(scopes))
	for i, sc := range scopes {
		placeholders[i] = "?"
		args[i] = sc
	}
	return strings.Join(placeholders, ", "), args
}

func scanMemoryRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]MemoryEntry, error) {
	var out []MemoryEntry
	for rows.Next() {
		var e MemoryEntry
		var pinnedInt int
		if err := rows.Scan(&e.ID, &e.Scope, &e.Content, &pinnedInt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning memory row: %w", err)
		}
		e.Pinned = pinnedInt != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
