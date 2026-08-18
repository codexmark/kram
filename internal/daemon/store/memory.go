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

// UpdateMemoryEntry replaces one entry's content in place, keeping its ID
// and created_at — the "replace" half of the add/replace/remove trio the
// agent needs to consolidate memory when it hits the size cap.
func (s *Store) UpdateMemoryEntry(id int64, content string) error {
	res, err := s.db.Exec(`UPDATE memory_entries SET content = ?, updated_at = ? WHERE id = ?`, content, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("updating memory entry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking updated memory entry: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no memory entry with id %d", id)
	}
	return nil
}

// DeleteMemoryEntry removes one entry by ID.
func (s *Store) DeleteMemoryEntry(id int64) error {
	res, err := s.db.Exec(`DELETE FROM memory_entries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting memory entry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking deleted memory entry: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no memory entry with id %d", id)
	}
	return nil
}

// ScopeMemory returns every entry in one scope, pinned first then newest,
// with no limit — used to enforce the per-scope size cap and to show the
// agent everything it has to work with when consolidating.
func (s *Store) ScopeMemory(scope string) ([]MemoryEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, scope, content, pinned, created_at, updated_at FROM memory_entries
		 WHERE scope = ? ORDER BY pinned DESC, updated_at DESC`,
		scope,
	)
	if err != nil {
		return nil, fmt.Errorf("listing scope memory: %w", err)
	}
	defer rows.Close()
	return scanMemoryRows(rows)
}

// ScopeMemorySize is the total character count of a scope's memory —
// what the cap is enforced against.
func (s *Store) ScopeMemorySize(scope string) (int, error) {
	var total *int
	if err := s.db.QueryRow(`SELECT SUM(LENGTH(content)) FROM memory_entries WHERE scope = ?`, scope).Scan(&total); err != nil {
		return 0, fmt.Errorf("measuring scope memory: %w", err)
	}
	if total == nil {
		return 0, nil // no rows in this scope yet
	}
	return *total, nil
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
