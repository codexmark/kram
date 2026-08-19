package session

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/daemon/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCreateReturnsANewSession(t *testing.T) {
	s := New(newTestStore(t))
	sess, err := s.Create("my session")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title != "my session" {
		t.Errorf("Title = %q, want %q", sess.Title, "my session")
	}
	if !strings.HasPrefix(sess.ID, "ses_") {
		t.Errorf("ID = %q, want it to start with \"ses_\"", sess.ID)
	}
}

func TestListReturnsCreatedSessions(t *testing.T) {
	s := New(newTestStore(t))
	if _, err := s.Create("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("second"); err != nil {
		t.Fatal(err)
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List() = %+v, want 2 sessions", list)
	}
}

func TestGetReturnsSessionAndMessages(t *testing.T) {
	st := newTestStore(t)
	s := New(st)
	created, err := s.Create("with messages")
	if err != nil {
		t.Fatal(err)
	}

	sess, msgs, err := s.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != created.ID {
		t.Errorf("Get returned session %q, want %q", sess.ID, created.ID)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no messages for a freshly created session, got %+v", msgs)
	}
}

func TestGetReturnsErrNotFoundForUnknownID(t *testing.T) {
	s := New(newTestStore(t))
	_, _, err := s.Get("ses_does_not_exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestNewIDHasExpectedPrefixAndIsUnique(t *testing.T) {
	a := NewID()
	b := NewID()
	if !strings.HasPrefix(a, "ses_") || !strings.HasPrefix(b, "ses_") {
		t.Errorf("NewID() = %q, %q — want both prefixed with \"ses_\"", a, b)
	}
	if a == b {
		t.Error("two calls to NewID produced the same value")
	}
}
