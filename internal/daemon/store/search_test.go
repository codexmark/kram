package store

import (
	"testing"
)

// seedSession creates a session and appends msgs to it in order, returning
// the appended messages (with their assigned ids/timestamps).
func seedSession(t *testing.T, s *Store, id, title string, msgs []Message) []Message {
	t.Helper()
	if _, err := s.CreateSession(id, title); err != nil {
		t.Fatalf("creating session %s: %v", id, err)
	}
	var out []Message
	for _, m := range msgs {
		appended, err := s.AppendMessage(id, m)
		if err != nil {
			t.Fatalf("appending message to %s: %v", id, err)
		}
		out = append(out, appended)
	}
	return out
}

func TestSearchMessagesFindsRealUserText(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "s1", "working on the gateway", []Message{
		{Role: "user", Content: "how does the circuit breaker decide when to fall back to the next provider"},
		{Role: "assistant", Content: "it trips after a run of consecutive failures and retries the primary after a cooldown"},
	})

	results, err := s.SearchMessages("circuit breaker", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Message.Role != "user" || results[0].Message.Content == "" {
		t.Errorf("expected the real user message to match, got %+v", results[0].Message)
	}
}

func TestSearchMessagesNoMatchReturnsEmptyNotError(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "s1", "session", []Message{
		{Role: "user", Content: "let's talk about deployment"},
	})

	results, err := s.SearchMessages("nonexistentxyzterm", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected no matches, got %+v", results)
	}
}

func TestSearchMessagesRankingPrefersRelevantAndRecent(t *testing.T) {
	s := newTestStore(t)
	// An older session that only mentions "deploy" once, in passing.
	seedSession(t, s, "old", "old session", []Message{
		{Role: "user", Content: "unrelated question about formatting code"},
		{Role: "assistant", Content: "sure, here's how to format it. by the way the deploy step is separate."},
	})
	// A newer session that's centrally about deploy — should score higher
	// on BM25 (term appears more, document is more focused) and is more
	// recent, so it must not lose to the older, weaker mention.
	seedSession(t, s, "new", "new session", []Message{
		{Role: "user", Content: "walk me through the deploy process end to end, deploy deploy"},
	})

	results, err := s.SearchMessages("deploy", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 matches, got %d: %+v", len(results), results)
	}
	if results[0].Message.SessionID != "new" {
		t.Errorf("expected the more relevant/recent match first, got session %q first", results[0].Message.SessionID)
	}
}

// TestSearchMessagesTieBreaksNearEqualRelevanceByRecency exercises the
// actual near-tie path (applyRecencyTieBreak / closeScores), unlike the
// test above where the two matches genuinely differ in relevance. Two
// messages with the same single occurrence of the search term, same
// length, produce essentially identical bm25 scores; created_at is
// backdated directly via SQL (AppendMessage only offers second-resolution
// "now", too coarse to produce a real ordering within one fast test) so
// the two matches are distinguishable by recency alone.
func TestSearchMessagesTieBreaksNearEqualRelevanceByRecency(t *testing.T) {
	s := newTestStore(t)
	older := seedSession(t, s, "older", "older session", []Message{
		{Role: "user", Content: "tiebreak-term appears once here"},
	})
	seedSession(t, s, "newer", "newer session", []Message{
		{Role: "user", Content: "tiebreak-term shows up here"},
	})

	if _, err := s.db.Exec(`UPDATE messages SET created_at = ? WHERE id = ?`, older[0].CreatedAt-3600, older[0].ID); err != nil {
		t.Fatal(err)
	}

	results, err := s.SearchMessages("tiebreak-term", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if !closeScores(results[0].Score, results[1].Score) {
		t.Fatalf("test setup assumption broken: scores aren't near-tied (%v vs %v) — recency tie-break wasn't actually exercised", results[0].Score, results[1].Score)
	}
	if results[0].Message.SessionID != "newer" {
		t.Errorf("expected the more recent match to win a near-tie, got session %q first", results[0].Message.SessionID)
	}
}

func TestSearchMessagesAnchoredWindow(t *testing.T) {
	s := newTestStore(t)
	msgs := seedSession(t, s, "s1", "session", []Message{
		{Role: "user", Content: "message one"},
		{Role: "assistant", Content: "message two"},
		{Role: "user", Content: "message three"},
		{Role: "assistant", Content: "message four unique-search-term"},
		{Role: "user", Content: "message five"},
		{Role: "assistant", Content: "message six"},
		{Role: "user", Content: "message seven"},
	})

	results, err := s.SearchMessages("unique-search-term", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	window := results[0].Window
	// 3 before, the match, 3 after = 7, but only 3 exist before and 3 after
	// here since the match is message 4 of 7 — exactly enough on both sides.
	wantContents := []string{
		"message one", "message two", "message three",
		"message four unique-search-term",
		"message five", "message six", "message seven",
	}
	if len(window) != len(wantContents) {
		t.Fatalf("window has %d messages, want %d: %+v", len(window), len(wantContents), window)
	}
	for i, w := range window {
		if w.Content != wantContents[i] {
			t.Errorf("window[%d] = %q, want %q", i, w.Content, wantContents[i])
		}
	}
	// The matched message must be identifiable by id within its own window.
	if window[3].ID != results[0].Message.ID {
		t.Errorf("expected the matched message at window position 3, got id %d vs match id %d", window[3].ID, results[0].Message.ID)
	}
	_ = msgs
}

func TestSearchMessagesWindowTruncatesAtSessionEdges(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "s1", "session", []Message{
		{Role: "user", Content: "edge-term-start first message"},
		{Role: "assistant", Content: "second"},
	})

	results, err := s.SearchMessages("edge-term-start", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	// No messages precede the first one in the session — the window must
	// not error or wrap into anything else, just have nothing before it.
	if len(results[0].Window) != 2 {
		t.Fatalf("window = %+v, want 2 messages (the match plus its one follower)", results[0].Window)
	}
	if results[0].Window[0].Content != "edge-term-start first message" {
		t.Errorf("expected the match to be first in its window, got %+v", results[0].Window[0])
	}
}

func TestSearchMessagesDoesNotLeakAcrossSessions(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "session-a", "A", []Message{
		{Role: "user", Content: "alpha before"},
		{Role: "assistant", Content: "alpha shared-term match"},
		{Role: "user", Content: "alpha after"},
	})
	seedSession(t, s, "session-b", "B", []Message{
		{Role: "user", Content: "beta before"},
		{Role: "assistant", Content: "beta shared-term match"},
		{Role: "user", Content: "beta after"},
	})

	results, err := s.SearchMessages("shared-term", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		for _, w := range r.Window {
			if w.SessionID != r.Message.SessionID {
				t.Errorf("window for match in session %q contains a message from session %q: %+v",
					r.Message.SessionID, w.SessionID, w)
			}
		}
	}
}

func TestSearchMessagesExcludesSubagentSessionsByDefault(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "real", "chatting with the user", []Message{
		{Role: "user", Content: "let's discuss the widget-term feature"},
	})
	seedSession(t, s, "sub", "subagent: implement the widget-term feature", []Message{
		{Role: "user", Content: "goal: implement widget-term feature"},
	})

	defaultResults, err := s.SearchMessages("widget-term", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultResults) != 1 {
		t.Fatalf("expected subagent session excluded by default, got %d results: %+v", len(defaultResults), defaultResults)
	}
	if defaultResults[0].Message.SessionID != "real" {
		t.Errorf("expected the real session's match, got %+v", defaultResults[0])
	}

	allResults, err := s.SearchMessages("widget-term", 10, SearchScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(allResults) != 2 {
		t.Fatalf("expected both sessions with scope=all, got %d results: %+v", len(allResults), allResults)
	}
	var sawSubagent bool
	for _, r := range allResults {
		if r.Message.SessionID == "sub" {
			sawSubagent = true
			if !r.IsSubagent {
				t.Error("expected the subagent session's result to be flagged IsSubagent")
			}
		}
	}
	if !sawSubagent {
		t.Error("expected the subagent session's message when scope=all")
	}
}

// TestSearchMessagesExcludesCompactionSummaries lives in
// search_compaction_test.go (package store_test, an external test package)
// so it can import internal/daemon/compaction for the real
// CompactionMarkerName without creating an import cycle — compaction
// itself imports store, so an internal (package store) test file can't
// import compaction.

func TestSearchMessagesUnicode(t *testing.T) {
	s := newTestStore(t)
	seedSession(t, s, "s1", "sessão", []Message{
		{Role: "user", Content: "vamos discutir a configuração do daemon em produção"},
		{Role: "assistant", Content: "claro, começamos pelo arquivo de configuração"},
		{Role: "user", Content: "日本語のテストメッセージです"},
	})

	results, err := s.SearchMessages("configuração", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 matches for accented term, got %d: %+v", len(results), results)
	}

	// Non-Latin script should not error, even if the default FTS5
	// tokenizer's match quality for it is limited.
	if _, err := s.SearchMessages("日本語", 10, SearchScopeUser); err != nil {
		t.Fatalf("unicode (non-Latin) search must not error: %v", err)
	}
}

func TestSearchMessagesEmptyQuery(t *testing.T) {
	s := newTestStore(t)
	results, err := s.SearchMessages("", 10, SearchScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Errorf("expected nil results for an empty query, got %+v", results)
	}
}
