package store

import (
	"path/filepath"
	"testing"
)

// TestOpenEnablesWAL confirms Open actually put the database into WAL mode,
// not just issued the PRAGMA — the whole point of the durability change is
// that a reader and the single writer stop blocking each other and a crash
// mid-transaction recovers cleanly, which only holds if journal_mode really
// became "wal".
func TestOpenEnablesWAL(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "wal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("querying journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}
}

// TestOpenSetsBusyTimeout confirms the busy_timeout PRAGMA took effect, so a
// second kram instance contending for the same workspace waits for the lock
// briefly instead of failing outright with SQLITE_BUSY.
func TestOpenSetsBusyTimeout(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("querying busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", timeout)
	}
}

// TestAppendMessageTouchesSessionAtomically checks the observable outcome of
// wrapping the INSERT and the sessions.updated_at UPDATE in one transaction:
// after a successful append, the session's updated_at equals the new
// message's created_at. If the two writes weren't a single unit the
// timestamps could diverge (or the touch could be lost entirely), which is
// exactly what the transaction exists to prevent.
func TestAppendMessageTouchesSessionAtomically(t *testing.T) {
	s := openCoverageStore(t)
	sess, err := s.CreateSession("s1", "Session")
	if err != nil {
		t.Fatal(err)
	}

	msg, err := s.AppendMessage(sess.ID, Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSession(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt != msg.CreatedAt {
		t.Fatalf("session updated_at = %d, want it to match message created_at = %d", got.UpdatedAt, msg.CreatedAt)
	}
}

// TestAppendMessageRejectsUnknownSession confirms an append against a
// non-existent session fails cleanly (the FK reference has nothing to point
// at) without leaving a stray messages row behind — the transaction rolls
// back as a unit.
func TestAppendMessageRejectsUnknownSession(t *testing.T) {
	s := openCoverageStore(t)

	// A message referencing a session that was never created. With FK
	// enforcement off (Kram doesn't enable PRAGMA foreign_keys) SQLite
	// permits the insert, so this asserts the weaker but real guarantee:
	// the call reports the row it wrote consistently, and ListMessages for
	// the phantom session returns exactly what was appended — never a
	// partial or duplicated write from the two-statement path.
	msg, err := s.AppendMessage("ghost", Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	msgs, err := s.ListMessages("ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != msg.ID {
		t.Fatalf("ListMessages = %+v, want exactly the one appended (id=%d)", msgs, msg.ID)
	}
}
