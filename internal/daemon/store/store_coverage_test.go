package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func openCoverageStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSessionAndMessageRoundTrip(t *testing.T) {
	s := openCoverageStore(t)
	first, err := s.CreateSession("one", "First")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession("two", "Second"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession("one", "duplicate"); err == nil {
		t.Fatal("duplicate session succeeded")
	}

	sessions, err := s.ListSessions()
	if err != nil || len(sessions) != 2 {
		t.Fatalf("ListSessions = %+v, %v", sessions, err)
	}
	gotSession, err := s.GetSession(first.ID)
	if err != nil || gotSession.Title != first.Title {
		t.Fatalf("GetSession = %+v, %v", gotSession, err)
	}
	if _, err := s.GetSession("missing"); err != sql.ErrNoRows {
		t.Fatalf("missing err = %v", err)
	}

	want := Message{Role: "assistant", Content: "hello", Provider: "p", ToolCallID: "parent", Name: "tool", Images: []string{"data:image/png;base64,AA"}, ToolCalls: []openai.ToolCall{{ID: "tc1", Type: "function", Function: openai.ToolCallFunction{Name: "read", Arguments: `{}`}}}}
	appended, err := s.AppendMessage(first.ID, want)
	if err != nil {
		t.Fatal(err)
	}
	if appended.ID == 0 || appended.SessionID != first.ID || appended.CreatedAt == 0 {
		t.Fatalf("AppendMessage = %+v", appended)
	}
	messages, err := s.ListMessages(first.ID)
	if err != nil || len(messages) != 1 {
		t.Fatalf("ListMessages = %+v, %v", messages, err)
	}
	got := messages[0]
	if got.Content != want.Content || len(got.Images) != 1 || len(got.ToolCalls) != 1 || got.ToolCalls[0].Function.Name != "read" {
		t.Fatalf("round trip = %+v", got)
	}
	if empty, err := s.ListMessages("missing"); err != nil || len(empty) != 0 {
		t.Fatalf("missing messages = %+v, %v", empty, err)
	}
}

func TestDecodeMessageJSONFailures(t *testing.T) {
	for _, tc := range []struct{ tools, images string }{{`{`, ""}, {"", `{`}} {
		if err := decodeMessageJSON(&Message{}, tc.tools, tc.images); err == nil {
			t.Errorf("decodeMessageJSON(%q,%q) succeeded", tc.tools, tc.images)
		}
	}
}

func TestStoreMethodsFailAfterClose(t *testing.T) {
	s := openCoverageStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession("x", "x"); err == nil {
		t.Error("CreateSession succeeded")
	}
	if _, err := s.ListSessions(); err == nil {
		t.Error("ListSessions succeeded")
	}
	if _, err := s.GetSession("x"); err == nil {
		t.Error("GetSession succeeded")
	}
	if _, err := s.ListMessages("x"); err == nil {
		t.Error("ListMessages succeeded")
	}
	if _, err := s.AppendMessage("x", Message{Role: "user"}); err == nil {
		t.Error("AppendMessage succeeded")
	}
	if _, err := s.WriteMemoryEntry("scope", "x", false); err == nil {
		t.Error("WriteMemoryEntry succeeded")
	}
	if err := s.UpdateMemoryEntry(1, "x"); err == nil {
		t.Error("UpdateMemoryEntry succeeded")
	}
	if err := s.DeleteMemoryEntry(1); err == nil {
		t.Error("DeleteMemoryEntry succeeded")
	}
	if _, err := s.ScopeMemory("scope"); err == nil {
		t.Error("ScopeMemory succeeded")
	}
	if _, err := s.ScopeMemorySize("scope"); err == nil {
		t.Error("ScopeMemorySize succeeded")
	}
	if _, err := s.RecentMemory([]string{"scope"}, 1); err == nil {
		t.Error("RecentMemory succeeded")
	}
	if _, err := s.SearchMemory([]string{"scope"}, "x", 1); err == nil {
		t.Error("SearchMemory succeeded")
	}
	if _, err := s.SearchMessages("x", 1, "all"); err == nil {
		t.Error("SearchMessages succeeded")
	}
	if _, err := s.contextWindow("x", 1, 1, 1); err == nil {
		t.Error("contextWindow succeeded")
	}
}

func TestMemoryNoopAndMissingCases(t *testing.T) {
	s := openCoverageStore(t)
	if err := s.UpdateMemoryEntry(999, "x"); err == nil {
		t.Error("updating missing memory succeeded")
	}
	if err := s.DeleteMemoryEntry(999); err == nil {
		t.Error("deleting missing memory succeeded")
	}
	if n, err := s.ScopeMemorySize("none"); err != nil || n != 0 {
		t.Fatalf("empty size = %d, %v", n, err)
	}
	if got, err := s.RecentMemory(nil, 1); err != nil || got != nil {
		t.Fatalf("RecentMemory nil = %+v, %v", got, err)
	}
	if got, err := s.SearchMemory([]string{"s"}, "  ", 1); err != nil || got != nil {
		t.Fatalf("SearchMemory blank = %+v, %v", got, err)
	}
}

func TestOpenRejectsUnusableDatabasePath(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("Open(directory) succeeded")
	}
}
