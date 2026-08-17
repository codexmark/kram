// Package server exposes kram-gateway's OpenAI-compatible HTTP API and
// wires together routing, circuit breaking and telemetry for each request.
// The top-level handler always recovers from panics: a bug or an upstream
// surprise in one request must never take the process down.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/codexmark/kram-gateway/internal/breaker"
	"github.com/codexmark/kram-gateway/internal/config"
	"github.com/codexmark/kram-gateway/internal/openai"
	"github.com/codexmark/kram-gateway/internal/provider"
	"github.com/codexmark/kram-gateway/internal/router"
	"github.com/codexmark/kram-gateway/internal/telemetry"
)

// Server holds everything a request handler needs.
type Server struct {
	cfg       *config.Config
	providers map[string]provider.Provider
	router    *router.Router
	breakers  *breaker.Registry
	telemetry *telemetry.Registry
	logger    *slog.Logger
}

// New builds a Server from already-constructed dependencies.
func New(cfg *config.Config, providers map[string]provider.Provider, rt *router.Router, breakers *breaker.Registry, tel *telemetry.Registry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, providers: providers, router: rt, breakers: breakers, telemetry: tel, logger: logger}
}

// Handler returns the fully wired HTTP handler, including panic recovery.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /admin/status", s.handleStatus)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	return s.recoverMiddleware(s.logMiddleware(mux))
}

// recoverMiddleware guarantees that a panic anywhere in the request path
// becomes a 500 response instead of crashing the gateway process.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered", "panic", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("internal error: %v", rec))
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

type statusProvider struct {
	ID          string                  `json:"id"`
	Kind        string                  `json:"kind"`
	BreakerOpen bool                    `json:"breaker_open"`
	Stats       telemetry.ProviderStats `json:"stats"`
}

type statusResponse struct {
	Providers []statusProvider `json:"providers"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snapshot := s.telemetry.Snapshot()

	resp := statusResponse{Providers: make([]statusProvider, 0, len(s.providers))}
	for id, p := range s.providers {
		resp.Providers = append(resp.Providers, statusProvider{
			ID:          id,
			Kind:        p.Kind(),
			BreakerOpen: s.breakers.IsOpen(id),
			Stats:       snapshot[id],
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openai.ErrorResponse{
		Error: openai.ErrorBody{Message: message, Type: "kram_gateway_error"},
	})
}
