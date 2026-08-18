package store_test

// External test package (store_test, not store): this test needs
// internal/daemon/compaction for the real CompactionMarkerName value, and
// compaction imports store — an internal (package store) test file
// importing compaction would be a real import cycle. Living outside the
// package avoids that while still exercising the exact constant compaction
// uses to tag a summary message, not a hand-copied string that could drift.

import (
	"path/filepath"
	"testing"

	"github.com/codexmark/kram-gateway/internal/daemon/compaction"
	"github.com/codexmark/kram-gateway/internal/daemon/store"
)

func TestSearchMessagesExcludesCompactionSummaries(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	sessionID := "s1"
	if _, err := s.CreateSession(sessionID, "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(sessionID, store.Message{Role: "user", Content: "real message about horizon-term topic"}); err != nil {
		t.Fatal(err)
	}
	// A compaction summary is a role:"system" message tagged with
	// compaction.CompactionMarkerName — machine-generated text, not
	// something the user or model actually said.
	if _, err := s.AppendMessage(sessionID, store.Message{
		Role:    "system",
		Name:    compaction.CompactionMarkerName,
		Content: "PRIOR SESSION CONTEXT — reference only. horizon-term was discussed at length.",
	}); err != nil {
		t.Fatal(err)
	}

	results, err := s.SearchMessages("horizon-term", 10, store.SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected only the real user message to match, got %d: %+v", len(results), results)
	}
	if results[0].Message.Role != "user" {
		t.Errorf("expected the match to be the real user message, got role %q", results[0].Message.Role)
	}
	for _, r := range results {
		if r.Message.Role == "system" && r.Message.Name == compaction.CompactionMarkerName {
			t.Error("a compaction summary must never be returned as the matched message")
		}
	}
}
