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

	"github.com/codexmark/kram/internal/daemon/agent"
	"github.com/codexmark/kram/internal/daemon/session"
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
	mux.HandleFunc("POST /sessions/{id}/answer", s.handleAnswerQuestion)
	mux.HandleFunc("POST /sessions/{id}/approve", s.handleAnswerApproval)
	mux.HandleFunc("GET /tools", s.handleListTools)
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
		case agent.EventQuestion:
			writeEvent(map[string]any{"type": "question", "question_id": evt.QuestionID, "question": evt.Question, "options": evt.Options})
		case agent.EventApproval:
			writeEvent(map[string]any{
				"type": "approval", "approval_id": evt.ApprovalID, "tool": evt.ApprovalTool,
				"subject": evt.ApprovalSubject, "options": []string{"once", "always", "deny"},
			})
		case agent.EventRouteStart:
			writeEvent(map[string]any{"type": "route_start"})
		case agent.EventRouteDone:
			writeEvent(map[string]any{"type": "route_done", "route_call": evt.RouteCall})
		case agent.EventHeartbeat:
			// Payload-free on purpose — see EventHeartbeat's doc comment.
			// Harmless for a client with no special handling for this
			// type: it still resets any "time since last event" clock the
			// same way every other frame on this stream already does.
			writeEvent(map[string]any{"type": "heartbeat"})
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
		"route_trace":   result.RouteTrace,
		"usage":         result.Usage,
		"tool_activity": result.ToolActivity,
		"compactions":   result.Compactions,
		"image_notice":  result.ImageNotice,
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

type answerQuestionRequest struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

// handleAnswerQuestion delivers a pending ask_question's answer — the
// {id} path segment is the session (kept for REST consistency and so
// nothing else needs a separate top-level route) but the lookup itself is
// by question_id, which is already globally unique.
func (s *Server) handleAnswerQuestion(w http.ResponseWriter, r *http.Request) {
	var req answerQuestionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.QuestionID == "" {
		writeError(w, http.StatusBadRequest, "question_id must not be empty")
		return
	}
	if !s.agent.AnswerQuestion(req.QuestionID, req.Answer) {
		writeError(w, http.StatusNotFound, "no pending question with that id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type answerApprovalRequest struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"` // "once", "always", or "deny"
}

// handleAnswerApproval delivers a pending permission-policy approval's
// decision — same shape as handleAnswerQuestion, but a distinct endpoint
// and a distinct pending-id space (see agent.Service.pendingApprovals):
// an approval is a different kind of pause than ask_question, and mixing
// their id spaces would let an answer meant for one satisfy the other.
func (s *Server) handleAnswerApproval(w http.ResponseWriter, r *http.Request) {
	var req answerApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.ApprovalID == "" {
		writeError(w, http.StatusBadRequest, "approval_id must not be empty")
		return
	}
	if !s.agent.AnswerApproval(req.ApprovalID, req.Decision) {
		writeError(w, http.StatusNotFound, "no pending approval with that id, or decision was not one of once/always/deny")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleListTools reports every registered tool and skill, enabled or
// not — the CLI's tools/skills toggle screen builds its list from this
// rather than hardcoding a duplicate of the daemon's own registry.
func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tools":  s.agent.Tools(),
		"skills": s.agent.Skills(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
