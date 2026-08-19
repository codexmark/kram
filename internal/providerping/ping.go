// Package providerping does the minimum possible real check of whether a
// configured provider account is actually reachable and authorized right
// now — a "list models" call (the same convention nearly every one of
// these APIs supports), never a real completion request. It exists so the
// CLI's accounts screen can show a real, current status instead of just
// "a key is set" — a key can be present and still be rate-limited, revoked,
// or pointed at a provider having an outage, and a chat request is the
// only way to actually find that out before this existed.
package providerping

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Status is a ping's outcome, coarse enough to map directly to a colored
// dot in the UI.
type Status int

const (
	StatusUnknown  Status = iota // never pinged yet
	StatusOK                     // responded quickly and successfully
	StatusDegraded               // responded, but slowly, or with a soft issue
	StatusDown                   // auth failure, rate-limited, unreachable, or a server error
)

// degradedLatency is the threshold past which an otherwise-successful
// ping is reported as degraded rather than healthy — high latency right
// now is exactly the kind of real, current signal a static "key is set"
// check can't provide.
const degradedLatency = 1500 * time.Millisecond

// pingTimeout bounds how long one check can take — this runs on demand
// from a UI screen, not in the background, so it must not hang.
const pingTimeout = 6 * time.Second

// Result is one provider's current, real status.
type Result struct {
	Status  Status
	Latency time.Duration
	// Detail is a short, human-readable explanation — empty for a clean
	// StatusOK, set for anything else ("429 sem cota", "401 chave
	// inválida", "latência alta", ...).
	Detail string
}

// Ping makes one minimal request against kind's API (anthropic, gemini,
// openai-compat, or openai-responses) using apiKey, and classifies the
// result. baseURL, if empty, uses each kind's public default — the same
// convention internal/provider's adapters already use. Every kind here
// has exactly one real auth shape (see internal/oauthflow/anthropic.go's
// doc comment for why an Anthropic account connected via browser login
// still ends up with a plain, permanent API key rather than needing a
// second auth mode here).
func Ping(ctx context.Context, kind, baseURL, apiKey string) Result {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	req, err := buildPingRequest(ctx, kind, baseURL, apiKey)
	if err != nil {
		return Result{Status: StatusDown, Detail: "requisição inválida: " + err.Error()}
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return Result{Status: StatusDown, Latency: elapsed, Detail: "sem resposta"}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return Result{Status: StatusDown, Latency: elapsed, Detail: "sem cota (429)"}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Result{Status: StatusDown, Latency: elapsed, Detail: "chave inválida"}
	case resp.StatusCode >= 500:
		return Result{Status: StatusDown, Latency: elapsed, Detail: "erro do provider"}
	case resp.StatusCode >= 400:
		return Result{Status: StatusDown, Latency: elapsed, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	case elapsed >= degradedLatency:
		return Result{Status: StatusDegraded, Latency: elapsed, Detail: "latência alta"}
	default:
		return Result{Status: StatusOK, Latency: elapsed}
	}
}

// buildPingRequest mirrors each kind's real auth convention from
// internal/provider's adapters, but targets a cheap "list models"
// endpoint instead of a chat completion — no completion tokens spent,
// just connectivity/auth/rate-limit signal.
func buildPingRequest(ctx context.Context, kind, baseURL, apiKey string) (*http.Request, error) {
	switch kind {
	case "anthropic":
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return req, nil

	case "gemini":
		if baseURL == "" {
			baseURL = "https://generativelanguage.googleapis.com"
		}
		return http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1beta/models?key="+apiKey, nil)

	case "openai-responses":
		// The Codex/ChatGPT backend has no confirmed lightweight "list
		// models" endpoint (see internal/provider/openai_responses.go) —
		// this sends a request that's expected to be rejected as
		// malformed (empty input) rather than complete, and relies on
		// the backend checking auth before body shape: a 401/403 still
		// reads as "chave inválida" below, anything else as reachable.
		// Approximate by nature — a real completion is the only fully
		// reliable check for this backend.
		if baseURL == "" {
			baseURL = "https://chatgpt.com/backend-api/codex/responses"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, strings.NewReader(`{"model":"gpt-5.5","input":[],"stream":false}`))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		return req, nil

	default: // "openai-compat"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		return req, nil
	}
}
