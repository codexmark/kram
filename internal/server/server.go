// Package server exposes kram-gateway's OpenAI-compatible HTTP API and
// wires together routing, circuit breaking and telemetry for each request.
// The top-level handler always recovers from panics: a bug or an upstream
// surprise in one request must never take the process down.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/codexmark/kram/internal/breaker"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/provider"
	"github.com/codexmark/kram/internal/router"
	"github.com/codexmark/kram/internal/telemetry"
)

// Server holds everything a request handler needs.
type Server struct {
	cfg       *config.Config
	providers map[string]provider.Provider
	router    *router.Router
	breakers  *breaker.Registry
	telemetry *telemetry.Registry
	logger    *slog.Logger
	// configPath is where a persisted routing change (POST /admin/strategy
	// with persist:true) is written back. Empty means the gateway has no
	// on-disk config (a pure-autodetect run) — persist requests are then
	// rejected rather than silently dropped.
	configPath string
}

// New builds a Server from already-constructed dependencies. configPath is
// the gateway's config.yaml (may be "" when none exists on disk).
func New(cfg *config.Config, configPath string, providers map[string]provider.Provider, rt *router.Router, breakers *breaker.Registry, tel *telemetry.Registry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, configPath: configPath, providers: providers, router: rt, breakers: breakers, telemetry: tel, logger: logger}
}

// Handler returns the fully wired HTTP handler, including panic recovery.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /admin/status", s.handleStatus)
	mux.HandleFunc("POST /admin/strategy", s.handleSetStrategy)
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
	ID             string                  `json:"id"`
	Kind           string                  `json:"kind"`
	BreakerOpen    bool                    `json:"breaker_open"`
	SupportsImages bool                    `json:"supports_images"`
	SupportsTools  bool                    `json:"supports_tools"`
	Stats          telemetry.ProviderStats `json:"stats"`
}

type statusCombo struct {
	ID        string   `json:"id"`
	Strategy  string   `json:"strategy"`
	Providers []string `json:"providers"`
}

type statusResponse struct {
	Providers  []statusProvider `json:"providers"`
	Combos     []statusCombo    `json:"combos"`
	Strategies []string         `json:"strategies"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snapshot := s.telemetry.Snapshot()

	resp := statusResponse{
		Providers:  make([]statusProvider, 0, len(s.providers)),
		Strategies: router.KnownStrategyNames(),
	}
	for id, p := range s.providers {
		resp.Providers = append(resp.Providers, statusProvider{
			ID:             id,
			Kind:           p.Kind(),
			BreakerOpen:    s.breakers.IsOpen(id),
			SupportsImages: p.SupportsImages(),
			SupportsTools:  p.SupportsTools(),
			Stats:          snapshot[id],
		})
	}

	for _, c := range s.router.Combos() {
		resp.Combos = append(resp.Combos, statusCombo{ID: c.ID, Strategy: c.Strategy, Providers: c.Providers})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

type setStrategyRequest struct {
	Combo    string `json:"combo"`
	Strategy string `json:"strategy"`
	// Persist, when true, writes the change through to config.yaml so it
	// survives a gateway restart (the runtime change alone is lost on
	// restart). MakeDefault additionally persists this combo as the config's
	// default_combo. Both require the gateway to have an on-disk config.
	Persist     bool `json:"persist,omitempty"`
	MakeDefault bool `json:"make_default,omitempty"`
}

// handleSetStrategy changes routing for future model calls without
// restarting the gateway. This is deliberately loopback-only: a gateway may
// expose its OpenAI-compatible endpoint to a LAN, but that must not also give
// remote callers an unauthenticated control plane.
func (s *Server) handleSetStrategy(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeError(w, http.StatusForbidden, "strategy changes are only allowed from localhost")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req setStrategyRequest
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid strategy request: "+err.Error())
		return
	}
	if req.Combo == "" || req.Strategy == "" {
		writeError(w, http.StatusBadRequest, "combo and strategy are required")
		return
	}
	if err := s.router.SetStrategy(req.Combo, req.Strategy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Persist || req.MakeDefault {
		if s.configPath == "" {
			writeError(w, http.StatusServiceUnavailable, "gateway has no config file to persist to")
			return
		}
		// The runtime change already applied; a persist failure is a partial
		// success, so say so explicitly rather than pretend nothing changed.
		if err := s.persistLiveConfig(req.MakeDefault, req.Combo); err != nil {
			writeError(w, http.StatusInternalServerError, "strategy changed at runtime but failed to persist: "+err.Error())
			return
		}
	}

	var updated statusCombo
	for _, c := range s.router.Combos() {
		if c.ID == req.Combo {
			updated = statusCombo{ID: c.ID, Strategy: c.Strategy, Providers: c.Providers}
			break
		}
	}
	s.logger.Info("routing strategy changed", "combo", req.Combo, "strategy", req.Strategy, "persisted", req.Persist, "made_default", req.MakeDefault)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}

// persistLiveConfig writes the gateway's live config to disk. Provider and
// combo *membership* come from s.cfg — the full, unsanitized live config,
// which includes providers that failed to build this boot and any reconciled
// additions — so persisting never silently drops them (unlike s.router.Combos(),
// which is sanitized). Only the per-combo Strategy is taken from the router,
// the authority for the current live strategy (capturing every prior runtime
// change, not just this one). Host/Port are restored from the on-disk file,
// because finalizeFileConfig rewrites them to an ephemeral runtime port for
// auto-discovered configs — writing s.cfg's port verbatim would clobber the
// file's real one.
func (s *Server) persistLiveConfig(makeDefault bool, defaultCombo string) error {
	toSave := *s.cfg
	toSave.Combos = append([]config.ComboConfig(nil), s.cfg.Combos...)
	for i := range toSave.Combos {
		if name := s.router.StrategyName(toSave.Combos[i].ID); name != "" {
			toSave.Combos[i].Strategy = name
		}
	}
	if makeDefault && defaultCombo != "" {
		toSave.DefaultCombo = defaultCombo
	}
	if onDisk, err := config.Load(s.configPath); err == nil {
		toSave.Host, toSave.Port = onDisk.Host, onDisk.Port
	} else {
		// No readable file yet (first write): use the documented default
		// port rather than the ephemeral runtime one so the saved config is
		// stable across boots.
		toSave.Host, toSave.Port = "", 0
	}
	return config.Save(&toSave, s.configPath)
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openai.ErrorResponse{
		Error: openai.ErrorBody{Message: message, Type: "kram_gateway_error"},
	})
}
