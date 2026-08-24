// Package statusclient is the CLI's HTTP client for a running
// kram-gateway's /admin/status: real per-provider telemetry and combo
// definitions, used to draw the footer and the expanded strategy panel.
// Nothing here is simulated — every number comes straight from the
// gateway's own counters.
package statusclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ProviderStats mirrors the gateway's telemetry.ProviderStats.
type ProviderStats struct {
	Requests         int64   `json:"requests"`
	Failures         int64   `json:"failures"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMS     int64   `json:"avg_latency_ms"`
	SuccessRate      float64 `json:"success_rate"`
}

// Provider is one entry in the gateway's status response.
type Provider struct {
	ID          string        `json:"id"`
	Kind        string        `json:"kind"`
	BreakerOpen bool          `json:"breaker_open"`
	Stats       ProviderStats `json:"stats"`
}

// Combo is one entry in the gateway's status response.
type Combo struct {
	ID        string   `json:"id"`
	Strategy  string   `json:"strategy"`
	Providers []string `json:"providers"`
}

// Status is the full /admin/status payload.
type Status struct {
	Providers  []Provider `json:"providers"`
	Combos     []Combo    `json:"combos"`
	Strategies []string   `json:"strategies"`
}

// Client talks to a kram-gateway instance's status endpoint.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a client pointed at a running gateway (e.g. http://127.0.0.1:20128).
func New(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

// Fetch retrieves the current status snapshot.
func (c *Client) Fetch(ctx context.Context) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/admin/status", nil)
	if err != nil {
		return Status{}, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("calling gateway: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return Status{}, fmt.Errorf("gateway returned %s", resp.Status)
	}

	var status Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return Status{}, fmt.Errorf("decoding gateway status: %w", err)
	}
	return status, nil
}

// SetStrategy changes combo's runtime strategy for future model calls. The
// gateway intentionally exposes this mutation only to loopback clients.
func (c *Client) SetStrategy(ctx context.Context, combo, strategy string) (Combo, error) {
	payload, err := json.Marshal(map[string]string{"combo": combo, "strategy": strategy})
	if err != nil {
		return Combo{}, fmt.Errorf("encoding strategy request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/admin/strategy", bytes.NewReader(payload))
	if err != nil {
		return Combo{}, fmt.Errorf("building strategy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Combo{}, fmt.Errorf("calling gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Error.Message != "" {
			return Combo{}, fmt.Errorf("gateway: %s", body.Error.Message)
		}
		return Combo{}, fmt.Errorf("gateway returned %s", resp.Status)
	}

	var updated Combo
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		return Combo{}, fmt.Errorf("decoding strategy response: %w", err)
	}
	return updated, nil
}
