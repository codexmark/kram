// Package server exposes the daemon's HTTP control surface: sessions are
// created, listed and messaged through here. Like the gateway, every
// handler runs behind a panic-recovery middleware — a single bad request
// must never take the daemon down and orphan every session it owns.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/daemon/agent"
	"github.com/codexmark/kram/internal/daemon/session"
)

// Server holds the session and agent services and exposes them over HTTP.
type Server struct {
	sessions  *session.Service
	agent     *agent.Service
	logger    *slog.Logger
	authToken string // required bearer token on every route except /health — see New and authMiddleware
	turns     *turnRegistry
}

// New builds a daemon Server. authToken is the per-process bearer token
// every route (except /health) requires — the daemon's HTTP surface drives
// real code execution (bash, edit_file, approving its own tool calls), so
// leaving it open would let any local process, or a browser tab via DNS
// rebinding, run code as the user. An empty authToken means "no auth"
// (only for tests / an explicitly-insecure standalone run) and logs a
// loud warning.
func New(sessions *session.Service, agentSvc *agent.Service, logger *slog.Logger, authToken string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if authToken == "" {
		logger.Warn("daemon HTTP server started with NO auth token — every local process can drive it; this should only happen in tests")
	}
	return &Server{sessions: sessions, agent: agentSvc, logger: logger, authToken: authToken, turns: newTurnRegistry()}
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
	mux.HandleFunc("GET /sessions/{id}/turn", s.handleAttachTurn)
	mux.HandleFunc("POST /sessions/{id}/interrupt", s.handleInterrupt)
	mux.HandleFunc("POST /sessions/{id}/steer", s.handleSteer)
	mux.HandleFunc("POST /sessions/{id}/answer", s.handleAnswerQuestion)
	mux.HandleFunc("POST /sessions/{id}/approve", s.handleAnswerApproval)
	mux.HandleFunc("GET /tools", s.handleListTools)
	mux.HandleFunc("PUT /tools/settings", s.handleUpdateToolSettings)
	mux.HandleFunc("GET /processes", s.handleListProcesses)
	mux.HandleFunc("GET /processes/{id}/output", s.handleProcessOutput)
	mux.HandleFunc("POST /combo", s.handleSetCombo)
	mux.HandleFunc("GET /rewind", s.handleRewindInfo)
	mux.HandleFunc("POST /rewind", s.handleRewind)
	// Order matters: recover (outermost) → log → host/auth gate → mux.
	return s.recoverMiddleware(s.logMiddleware(s.guardMiddleware(mux)))
}

// guardMiddleware is the daemon's HTTP perimeter: a Host-header check
// (cheap DNS-rebinding defense — a rebinding attack arrives with an
// attacker-controlled Host, not localhost) plus a constant-time bearer
// token check. /health is exempt (it exposes nothing and is used as a
// readiness probe before the client knows anything). The bearer token is
// itself the core defense: a cross-origin "simple" request from a browser
// cannot set an Authorization header without tripping a CORS preflight the
// daemon never answers, so this also closes the no-Content-Type-check
// simple-request vector the audit flagged.
func (s *Server) guardMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if !hostIsLocal(r.Host) {
			writeError(w, http.StatusForbidden, "requests must target localhost")
			return
		}
		if s.authToken != "" && !bearerTokenValid(r, s.authToken) {
			writeError(w, http.StatusUnauthorized, "missing or invalid daemon auth token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostIsLocal reports whether the request's Host header names a loopback
// address (or is empty/HTTP-1.0-style with no host). A DNS-rebinding
// attack reaches the daemon with the attacker's own hostname in Host, so
// rejecting anything non-loopback here blocks that class before the token
// check even runs.
func hostIsLocal(host string) bool {
	if host == "" {
		return true
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// bearerTokenValid does a constant-time compare of the request's bearer
// token against want, so a timing side channel can't be used to recover
// the token byte by byte.
func bearerTokenValid(r *http.Request, want string) bool {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, prefix)), []byte(want)) == 1
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

	// The run gets its own context, detached from this request: closing
	// the stream merely unsubscribes (the work continues server-side);
	// only POST /sessions/{id}/interrupt cancels it. One turn per session.
	runCtx, cancel := context.WithCancel(context.Background())
	t, started := s.turns.start(id, cancel)
	if !started {
		cancel()
		writeError(w, http.StatusConflict, "a turn is already running for this session — attach with GET /sessions/{id}/turn or interrupt it first")
		return
	}

	go func() {
		onEvent := func(evt agent.Event) {
			if frame, ok := eventFrame(evt); ok {
				t.publish(frame)
			}
		}
		result, err := s.agent.Run(runCtx, id, req.Content, req.Images, onEvent)
		cancel()
		if err != nil {
			t.finish(mustFrame(map[string]any{"type": "error", "error": err.Error()}))
		} else {
			t.finish(mustFrame(map[string]any{
				"type":          "done",
				"message":       result.Message,
				"attempts":      result.Attempts,
				"route_trace":   result.RouteTrace,
				"usage":         result.Usage,
				"tool_activity": result.ToolActivity,
				"compactions":   result.Compactions,
				"image_notice":  result.ImageNotice,
			}))
		}
		s.turns.retire(id, t)
	}()

	s.streamTurn(w, r, t)
}

// handleAttachTurn reattaches a client to the session's turn: full replay
// of everything published so far, then live until it finishes. 404 when
// no turn is active or recently finished — the session's persisted
// history is then the complete record.
func (s *Server) handleAttachTurn(w http.ResponseWriter, r *http.Request) {
	t := s.turns.get(r.PathValue("id"))
	if t == nil {
		writeError(w, http.StatusNotFound, "no active turn for this session")
		return
	}
	s.streamTurn(w, r, t)
}

type steerRequest struct {
	Content string `json:"content"`
}

// handleSteer queues a user message for the session's *running* turn —
// the agent drains it at the next model-call boundary. 409 when no turn
// is active: the client should send a normal message instead.
func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req steerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content must not be empty")
		return
	}
	t := s.turns.get(id)
	if t == nil || t.isDone() {
		writeError(w, http.StatusConflict, "no turn is running — send a normal message instead")
		return
	}
	s.agent.QueueSteering(id, req.Content)
	// Let every attached subscriber (including a future reattach's replay)
	// see the user's queued words in the turn's own stream.
	t.publish(mustFrame(map[string]any{"type": "steered", "content": req.Content}))
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

// handleInterrupt cancels the session's active turn — the explicit
// replacement for the old closing-the-stream-cancels contract.
func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	if !s.turns.interrupt(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "no active turn for this session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "interrupting"})
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

type updateToolSettingsRequest struct {
	Disabled []string `json:"disabled"`
}

func (s *Server) handleUpdateToolSettings(w http.ResponseWriter, r *http.Request) {
	var req updateToolSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	s.agent.ReplaceDisabledTools(req.Disabled)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type setComboRequest struct {
	Combo string `json:"combo"`
}

// handleSetCombo switches the combo future messages route to. Validated
// against the gateway's advertised combos (see agent.SetActiveCombo);
// in-flight turns keep the combo they started with.
func (s *Server) handleSetCombo(w http.ResponseWriter, r *http.Request) {
	var req setComboRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := s.agent.SetActiveCombo(r.Context(), req.Combo); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "combo": req.Combo})
}

type rewindRequest struct {
	ID string `json:"id"`
}

// handleRewindInfo reports the newest automatic pre-mutation checkpoint —
// what a one-key rewind would restore to — so the client can show it and
// confirm before the destructive POST below.
func (s *Server) handleRewindInfo(w http.ResponseWriter, r *http.Request) {
	snap, ok, err := s.agent.LatestAutoCheckpoint(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no automatic checkpoint exists yet — one is taken before a turn's first file change")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleRewind restores the workspace to the given snapshot id — the id
// is required (from a prior GET /rewind) so a slow confirmation can never
// restore a different checkpoint than the one the user was shown.
func (s *Server) handleRewind(w http.ResponseWriter, r *http.Request) {
	var req rewindRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required (from GET /rewind)")
		return
	}
	res, snap, err := s.agent.Rewind(r.Context(), req.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": res, "snapshot": snap})
}

func (s *Server) handleListProcesses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.agent.BackgroundProcesses())
}

func (s *Server) handleProcessOutput(w http.ResponseWriter, r *http.Request) {
	var cursor *int64
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
			return
		}
		cursor = &value
	}
	output, ok := s.agent.BackgroundProcessOutput(r.PathValue("id"), cursor)
	if !ok {
		writeError(w, http.StatusNotFound, "background process not found")
		return
	}
	writeJSON(w, http.StatusOK, output)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
