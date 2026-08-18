package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWriteAndRecentMemory(t *testing.T) {
	s := newTestStore(t)

	_, err := s.WriteMemoryEntry("proj", "the user prefers terse commit messages", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.WriteMemoryEntry(GlobalScope, "the user's name is Mark", true)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := s.RecentMemory([]string{"proj", GlobalScope}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	// Pinned entries sort first regardless of recency.
	if !entries[0].Pinned || entries[0].Content != "the user's name is Mark" {
		t.Errorf("pinned entry should sort first, got %+v", entries[0])
	}
}

func TestRecentMemoryRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		if _, err := s.WriteMemoryEntry("proj", "fact", false); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.RecentMemory([]string{"proj"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("got %d entries, want 3 (the limit)", len(entries))
	}
}

func TestRecentMemoryScopeIsolation(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.WriteMemoryEntry("project-a", "fact about A", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMemoryEntry("project-b", "fact about B", false); err != nil {
		t.Fatal(err)
	}

	entries, err := s.RecentMemory([]string{"project-a"}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Content != "fact about A" {
		t.Errorf("querying scope project-a should not see project-b's memory, got %+v", entries)
	}
}

func TestSearchMemoryFTS(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.WriteMemoryEntry("proj", "the deploy script lives in scripts/deploy.sh", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMemoryEntry("proj", "the user likes dark mode", false); err != nil {
		t.Fatal(err)
	}

	results, err := s.SearchMemory([]string{"proj"}, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Content != "the deploy script lives in scripts/deploy.sh" {
		t.Errorf("full-text search for 'deploy' should find exactly the deploy entry, got %+v", results)
	}
}

func TestUpdateMemoryEntry(t *testing.T) {
	s := newTestStore(t)
	entry, err := s.WriteMemoryEntry("proj", "old content", false)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateMemoryEntry(entry.ID, "new content"); err != nil {
		t.Fatal(err)
	}

	entries, _ := s.RecentMemory([]string{"proj"}, 8)
	if len(entries) != 1 || entries[0].Content != "new content" {
		t.Errorf("expected the entry's content to be replaced, got %+v", entries)
	}
	if entries[0].ID != entry.ID {
		t.Error("replace should keep the same id")
	}
}

func TestUpdateMemoryEntryUnknownID(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateMemoryEntry(99999, "x"); err == nil {
		t.Error("expected an error updating a nonexistent memory entry")
	}
}

func TestDeleteMemoryEntry(t *testing.T) {
	s := newTestStore(t)
	entry, _ := s.WriteMemoryEntry("proj", "temporary", false)

	if err := s.DeleteMemoryEntry(entry.ID); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.RecentMemory([]string{"proj"}, 8)
	if len(entries) != 0 {
		t.Errorf("expected no entries after delete, got %+v", entries)
	}
}

func TestDeleteMemoryEntryUnknownID(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteMemoryEntry(99999); err == nil {
		t.Error("expected an error deleting a nonexistent memory entry")
	}
}

func TestScopeMemorySize(t *testing.T) {
	s := newTestStore(t)

	size, err := s.ScopeMemorySize("proj")
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Errorf("empty scope should measure 0, got %d", size)
	}

	if _, err := s.WriteMemoryEntry("proj", "12345", false); err != nil { // 5 chars
		t.Fatal(err)
	}
	if _, err := s.WriteMemoryEntry("proj", "abc", false); err != nil { // 3 chars
		t.Fatal(err)
	}
	size, err = s.ScopeMemorySize("proj")
	if err != nil {
		t.Fatal(err)
	}
	if size != 8 {
		t.Errorf("scope size = %d, want 8 (5+3 chars)", size)
	}
}

func TestScopeMemory(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.WriteMemoryEntry("proj", "unpinned", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMemoryEntry("proj", "pinned", true); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ScopeMemory("proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if !entries[0].Pinned {
		t.Error("pinned entry should sort first in ScopeMemory too")
	}
}
