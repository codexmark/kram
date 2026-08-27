// Package customprovider is a small local store of user-registered
// OpenAI-compatible endpoints — the accounts screen's "+ adicionar
// provedor customizado" flow, for pointing Kram at a local or LAN server
// (llama.cpp, LM Studio, Ollama's OpenAI endpoint, vLLM,
// text-generation-webui — all speak the same chat-completions wire
// format internal/provider/openai_compat.go already talks) that the
// fixed internal/providercatalog list has no way to represent.
//
// Same on-disk shape and guarantees as internal/toolsettings/
// internal/onboarding: plain JSON under kramhome, 0600, no separate save
// step, "missing file" is the normal first-run state. The one thing this
// store deliberately does *not* hold is the API key itself — that's
// optional here (most local servers have no auth) and, when present,
// lives in internal/credentials.Store under this entry's own synthesized
// EnvVar, exactly the way an OAuth-connected account's synthetic env var
// already works. One place for every secret, not two.
package customprovider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codexmark/kram/internal/kramhome"
	"github.com/codexmark/kram/internal/localstore"
)

// Provider is one user-registered custom endpoint.
type Provider struct {
	ID      string `json:"id"`       // stable slug derived from Name, deduped on collision
	Name    string `json:"name"`     // display name, e.g. "Meu Servidor"
	BaseURL string `json:"base_url"` // e.g. "http://192.168.1.50:8080/v1"
	EnvVar  string `json:"env_var"`  // lookup key into credentials.Store for the (optional) API key
	// Model pins the upstream model ID, matching every other catalog
	// provider's DefaultModel. Required, unlike a catalog provider's
	// DefaultModel (which is always populated by Kram itself) — Add
	// rejects an empty value. Genuine passthrough (forwarding whatever
	// the request's own "model" field says) doesn't actually work today:
	// that field is the *combo ID* for any Kram-originated call, never a
	// real upstream model name, so an unpinned custom provider used to
	// silently receive something like "default" as its model — a real
	// bug, not a supported mode. See DECISIONS.md.
	Model string `json:"model"`
	// SupportsTools records whether this server accepts tool/function
	// definitions — a pointer so "never explicitly set" (every entry
	// created before this field existed) is distinguishable from "set to
	// false", matching AttemptInfo.Score's pattern elsewhere in this
	// codebase. Defaults to true (see SupportsToolsOrDefault) since most
	// OpenAI-compatible local servers (llama.cpp, LM Studio, vLLM,
	// Ollama) do support tool calling — this exists so a user whose
	// server genuinely can't has a way to say so, instead of Kram
	// silently sending tool definitions a server can't handle.
	SupportsTools *bool `json:"supports_tools,omitempty"`
	// ContextWindow is the model's total token window, used to size the
	// compaction budget (see config.Config.ComboContextWindow). Optional: 0
	// means "unknown" and is ignored when taking a combo's minimum window.
	// This matters most for local models — an 8K–32K local server otherwise
	// inherits Kram's much larger default budget and overflows its real
	// window every turn without compaction ever firing.
	ContextWindow int `json:"context_window,omitempty"`
}

// SupportsToolsOrDefault reports p.SupportsTools' value, or true if it
// was never explicitly set — see that field's doc comment.
func (p Provider) SupportsToolsOrDefault() bool {
	return p.SupportsTools == nil || *p.SupportsTools
}

// Store holds every registered custom provider, in registration order.
type Store struct {
	path    string
	entries []Provider
}

// Load reads the custom-providers file, or returns an empty Store if it
// doesn't exist yet.
func Load() (*Store, error) {
	path, err := kramhome.Path("custom_providers.json")
	if err != nil {
		return nil, err
	}
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, err
	}
	return s, nil
}

// All returns every registered custom provider, in registration order.
func (s *Store) All() []Provider {
	out := make([]Provider, len(s.entries))
	copy(out, s.entries)
	return out
}

// Add registers a new custom provider, deriving a stable ID (and the
// credentials-store EnvVar that goes with it) from name. name, baseURL
// and model are all required — see Provider.Model's doc comment for why
// an empty model isn't a supported "passthrough" mode. Persists
// immediately.
func (s *Store) Add(name, baseURL, model string, supportsTools bool, contextWindow int) (Provider, error) {
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	model = strings.TrimSpace(model)
	if name == "" {
		return Provider{}, fmt.Errorf("custom provider name is required")
	}
	if baseURL == "" {
		return Provider{}, fmt.Errorf("custom provider URL is required")
	}
	if model == "" {
		return Provider{}, fmt.Errorf("custom provider model is required — Kram cannot forward the combo's internal routing id upstream as a substitute")
	}
	if contextWindow < 0 {
		contextWindow = 0
	}

	id := s.uniqueID(slugify(name))
	p := Provider{
		ID:            id,
		Name:          name,
		BaseURL:       baseURL,
		EnvVar:        envVarFor(id),
		Model:         model,
		SupportsTools: &supportsTools,
		ContextWindow: contextWindow,
	}
	s.entries = append(s.entries, p)
	if err := s.save(); err != nil {
		return Provider{}, err
	}
	return p, nil
}

// Delete removes a custom provider by ID and persists immediately. Not
// an error if id isn't found — the accounts screen never offers a
// delete on a row that isn't there.
func (s *Store) Delete(id string) error {
	for i, p := range s.entries {
		if p.ID == id {
			s.entries = append(s.entries[:i:i], s.entries[i+1:]...)
			return s.save()
		}
	}
	return nil
}

func (s *Store) uniqueID(base string) string {
	id := base
	for n := 2; s.idExists(id); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

func (s *Store) idExists(id string) bool {
	for _, p := range s.entries {
		if p.ID == id {
			return true
		}
	}
	return false
}

// slugify turns a display name into a stable, URL/env-var-safe ID:
// lowercase, non-alphanumeric runs collapsed to a single "-", trimmed.
// Falls back to "provider" if that leaves nothing (an all-symbols name).
func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	id := strings.TrimSuffix(b.String(), "-")
	if id == "" {
		return "provider"
	}
	return id
}

// envVarFor synthesizes the credentials.Store lookup key for a custom
// provider's (optional) API key from its ID.
func envVarFor(id string) string {
	return "CUSTOM_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_API_KEY"
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return localstore.AtomicWrite(s.path, data, 0o600)
}
