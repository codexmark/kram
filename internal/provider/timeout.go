package provider

import "time"

// DefaultTimeout is the per-request HTTP timeout every adapter uses unless
// Build is handed an override. It covers the whole request — dial, headers,
// and reading the full body — so it must be generous enough for a slow but
// healthy local model; config.Tunables.ProviderTimeout raises or lowers it.
const DefaultTimeout = 120 * time.Second

// timeoutSetter is implemented by every adapter whose HTTP client's request
// timeout can be overridden after construction. Build uses it to apply the
// configured provider timeout without threading the value through all four
// constructors (and their ~40 test call sites) — the constructors keep
// their DefaultTimeout, and Build overrides it only when a non-zero value
// is configured.
type timeoutSetter interface {
	setTimeout(time.Duration)
}

func (p *OpenAICompatible) setTimeout(d time.Duration) { p.client.Timeout = d }
func (p *Anthropic) setTimeout(d time.Duration)        { p.client.Timeout = d }
func (p *Gemini) setTimeout(d time.Duration)           { p.client.Timeout = d }
func (p *OpenAIResponses) setTimeout(d time.Duration)  { p.client.Timeout = d }
