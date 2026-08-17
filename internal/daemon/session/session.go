// Package session is the daemon's core: it owns sessions and their message
// history, and is the only thing that talks to the gateway on their
// behalf. Clients (CLI, HTTP, future TUI) all read and write through this
// one service, so there's a single source of truth regardless of how many
// clients are attached or whether they've disconnected.
package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/codexmark/kram-gateway/internal/daemon/gatewayclient"
	"github.com/codexmark/kram-gateway/internal/daemon/store"
	"github.com/codexmark/kram-gateway/internal/openai"
)

// ErrNotFound is returned when a session ID doesn't exist.
var ErrNotFound = errors.New("session not found")

// Service orchestrates persistence (store) and model calls (gateway).
type Service struct {
	store   *store.Store
	gateway *gatewayclient.Client
	// model selects which gateway combo new messages are sent to.
	model string
}

// New builds a session service backed by st, calling gw for completions
// with the given model/combo.
func New(st *store.Store, gw *gatewayclient.Client, model string) *Service {
	return &Service{store: st, gateway: gw, model: model}
}

// Create starts a new, empty session.
func (s *Service) Create(title string) (store.Session, error) {
	return s.store.CreateSession(newID(), title)
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

// SendMessage appends a user message, sends the full history to the
// gateway, persists the assistant's reply, and returns it. The user
// message is durable the moment this call returns its first half — even
// if the gateway call fails, the daemon still owns the fact that the user
// said something.
func (s *Service) SendMessage(ctx context.Context, sessionID, content string) (store.Message, error) {
	if _, err := s.store.GetSession(sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Message{}, ErrNotFound
		}
		return store.Message{}, err
	}

	if _, err := s.store.AppendMessage(sessionID, "user", content); err != nil {
		return store.Message{}, fmt.Errorf("persisting user message: %w", err)
	}

	history, err := s.store.ListMessages(sessionID)
	if err != nil {
		return store.Message{}, fmt.Errorf("loading history: %w", err)
	}

	messages := make([]openai.ChatMessage, 0, len(history))
	for _, m := range history {
		messages = append(messages, openai.ChatMessage{Role: m.Role, Content: m.Content})
	}

	reply, err := s.gateway.ChatCompletion(ctx, s.model, messages)
	if err != nil {
		return store.Message{}, fmt.Errorf("gateway call failed: %w", err)
	}

	assistantMsg, err := s.store.AppendMessage(sessionID, "assistant", reply)
	if err != nil {
		return store.Message{}, fmt.Errorf("persisting assistant message: %w", err)
	}
	return assistantMsg, nil
}

func newID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "ses_" + hex.EncodeToString(buf)
}
