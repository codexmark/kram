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
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	// StatusOK, set for anything else ("no quota (429)", "invalid
	// key", "high latency", ...).
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
		return Result{Status: StatusDown, Detail: "invalid request: " + err.Error()}
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return Result{Status: StatusDown, Latency: elapsed, Detail: "no response"}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return Result{Status: StatusDown, Latency: elapsed, Detail: "no quota (429)"}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Result{Status: StatusDown, Latency: elapsed, Detail: "invalid key"}
	case resp.StatusCode >= 500:
		return Result{Status: StatusDown, Latency: elapsed, Detail: "provider error"}
	case kind == "openai-responses" && resp.StatusCode >= 400:
		// The Codex/ChatGPT backend validates auth *before* request-body
		// shape, and this probe deliberately sends a minimal, incomplete
		// body (see buildPingRequest) that the backend always rejects with a
		// body-validation 400 ("Store must be set to false" / "input
		// required"). Reaching that 400 means auth succeeded and the backend
		// is reachable — the only thing this probe can confirm short of a
		// real, quota-consuming completion. A genuine auth failure is the
		// 401/403 already handled above; treating this 400 as down was a
		// false-negative red dot on a working ChatGPT login.
		return Result{Status: StatusOK, Latency: elapsed}
	case resp.StatusCode >= 400:
		return Result{Status: StatusDown, Latency: elapsed, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	case elapsed >= degradedLatency:
		return Result{Status: StatusDegraded, Latency: elapsed, Detail: "high latency"}
	default:
		return Result{Status: StatusOK, Latency: elapsed}
	}
}

// ListModels queries baseURL's OpenAI-compatible "/models" endpoint and
// returns the model IDs it advertises, sorted — the exact same request
// Ping's "openai-compat" branch already makes for a connectivity check,
// just reading the body instead of discarding it. Lets a caller (the
// CLI's custom-provider form) offer a real, currently-available model to
// pick from instead of asking the user to type one by hand and find out
// later, at the first real turn, whether they got it right. apiKey may
// be empty for a no-auth local/LAN server, matching buildPingRequest's
// own "openai-compat" convention.
func ListModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("no response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("invalid key")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}

	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
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
		// reads as "invalid key" below, anything else as reachable.
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
		// A no-auth custom local/LAN server (internal/customprovider) has
		// no key to send — skip the header rather than a malformed
		// "Bearer " that would otherwise just be ignored.
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		return req, nil
	}
}
