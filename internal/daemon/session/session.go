// Package session owns session lifecycle (create/list/get) — the durable
// record a client attaches to. Turn-taking (sending a message and running
// the agent loop) lives in internal/daemon/agent, which reads and appends
// to the same store directly; keeping that logic out of this package
// avoids Service growing two unrelated responsibilities.
package session

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/codexmark/kram-gateway/internal/daemon/store"
)

// ErrNotFound is returned when a session ID doesn't exist.
var ErrNotFound = errors.New("session not found")

// Service manages session records.
type Service struct {
	store *store.Store
}

// New builds a session service backed by st.
func New(st *store.Store) *Service {
	return &Service{store: st}
}

// Create starts a new, empty session.
func (s *Service) Create(title string) (store.Session, error) {
	return s.store.CreateSession(NewID(), title)
}

// List returns every known session, most recently active first.
func (s *Service) List() ([]store.Session, error) {
	return s.store.ListSessions()
}

// Get returns a session and its full message history.
func (s *Service) Get(id string) (store.Session, []store.Message, error) {
	sess, err := s.store.GetSession(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Session{}, nil, ErrNotFound
		}
		return store.Session{}, nil, err
	}
	msgs, err := s.store.ListMessages(id)
	if err != nil {
		return store.Session{}, nil, err
	}
	return sess, msgs, nil
}

// NewID generates a session identifier.
func NewID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "ses_" + hex.EncodeToString(buf)
}
