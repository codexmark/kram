package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ProviderStatus is the capability/health subset of the gateway's
// /admin/status the daemon needs to make routing decisions (e.g. whether
// any provider in a combo can accept images).
type ProviderStatus struct {
	ID             string `json:"id"`
	BreakerOpen    bool   `json:"breaker_open"`
	SupportsImages bool   `json:"supports_images"`
	SupportsTools  bool   `json:"supports_tools"`
}

// ComboStatus mirrors the gateway's combo summary.
type ComboStatus struct {
	ID        string   `json:"id"`
	Strategy  string   `json:"strategy"`
	Providers []string `json:"providers"`
}

// Status is the subset of GET /admin/status the daemon consumes.
type Status struct {
	Providers []ProviderStatus `json:"providers"`
	Combos    []ComboStatus    `json:"combos"`
}

// Status fetches the gateway's current provider/combo status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/admin/status", nil)
	if err != nil {
		return Status{}, fmt.Errorf("building status request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("calling gateway status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return Status{}, fmt.Errorf("gateway status returned %s", resp.Status)
	}

	var status Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return Status{}, fmt.Errorf("decoding gateway status: %w", err)
	}
	return status, nil
}

// ComboSupportsImages reports whether at least one provider in the named
// combo accepts image input. Kram never assumes support — this is the
// check the agent loop makes before attaching an image to a request.
func (s Status) ComboSupportsImages(comboID string) bool {
	for _, c := range s.Combos {
		if c.ID != comboID {
			continue
		}
		byID := make(map[string]ProviderStatus, len(s.Providers))
		for _, p := range s.Providers {
			byID[p.ID] = p
		}
		for _, pid := range c.Providers {
			if byID[pid].SupportsImages {
				return true
			}
		}
		return false
	}
	return false
}
