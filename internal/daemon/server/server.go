// Package server exposes the daemon's HTTP control surface: sessions are
// created, listed and messaged through here. Like the gateway, every
// handler runs behind a panic-recovery middleware — a single bad request
// must never take the daemon down and orphan every session it owns.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/codexmark/kram-gateway/internal/daemon/agent"
	"github.com/codexmark/kram-gateway/internal/daemon/session"
)

// Server holds the session and agent services and exposes them over HTTP.
type Server struct {
	sessions *session.Service
	agent    *agent.Service
	logger   *slog.Logger
}

// New builds a daemon Server.
func New(sessions *session.Service, agentSvc *agent.Service, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{sessions: sessions, agent: agentSvc, logger: logger}
}

// Handler returns the fully wired HTTP handler, including panic recovery.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /sessions", s.handleCreateSession)
	mux.HandleFunc("GET /sessions", s.handleListSessions)
	mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	mux.HandleFunc("GET /sessions/{id}/context", s.handleGetContext)
	mux.HandleFunc("POST /sessions/{id}/messages", s.handleSendMessage)
	return s.recoverMiddleware(s.logMiddleware(mux))
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered", "panic", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createSessionRequest struct {
	Title string `json:"title"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	sess, err := s.sessions.Create(req.Title)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.sessions.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, messages, err := s.sessions.Get(id)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "messages": messages})
}

func (s *Server) handleGetContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	usage, err := s.agent.ContextUsage(r.Context(), id)
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

type sendMessageRequest struct {
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"` // data: URLs
}

// handleSendMessage always responds over SSE: a play-by-play of the agent
// loop (text deltas as the model generates them, tool start/result,
// notices) as they happen, ending in one "done" event with the same shape
// the old single-JSON response had. The CLI is effectively the only
// consumer, and it always wants the live view — a curl/script client
// still gets a well-formed event stream, just not a single JSON blob.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content must not be empty")
		return
	}

	// Checked before committing to the stream so an invalid session ID
	// still gets a proper 404 instead of a 200 wrapping an error event.
	if _, _, err := s.sessions.Get(id); err != nil {
		if errors.Is(err, session.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeEvent := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	onEvent := func(evt agent.Event) {
		switch evt.Kind {
		case agent.EventDelta:
			writeEvent(map[string]any{"type": "delta", "content": evt.Content})
		case agent.EventToolStart:
			writeEvent(map[string]any{"type": "tool_start", "name": evt.ToolName, "args": evt.ToolArgs})
		case agent.EventToolResult:
			writeEvent(map[string]any{"type": "tool_result", "name": evt.ToolName, "result": evt.ToolResult, "ok": evt.ToolOK})
		case agent.EventNotice:
			writeEvent(map[string]any{"type": "notice", "text": evt.Notice})
		}
	}

	result, err := s.agent.Run(r.Context(), id, req.Content, req.Images, onEvent)
	if err != nil {
		writeEvent(map[string]any{"type": "error", "error": err.Error()})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	writeEvent(map[string]any{
		"type":          "done",
		"message":       result.Message,
		"attempts":      result.Attempts,
		"usage":         result.Usage,
		"tool_activity": result.ToolActivity,
		"compactions":   result.Compactions,
		"image_notice":  result.ImageNotice,
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
