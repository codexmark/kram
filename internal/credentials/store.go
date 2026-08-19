// Package credentials is a small local store for provider API keys, so
// Kram's accounts screen has somewhere to put a key the user pastes in
// instead of asking them to export an env var by hand every session (the
// friction that motivated this package in the first place — env vars only
// take effect in the exact shell they were exported in, which is an easy
// thing to get wrong).
//
// This is OS-permission-based protection (like gh/aws CLI's own
// credential files), not encryption at rest — there's no key-management
// story for a local single-user CLI tool that would make "encrypted JSON
// readable only by code that also ships the decryption key" meaningfully
// safer than a 0600 file. Treat it the same way you'd treat
// ~/.aws/credentials.
package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/codexmark/kram/internal/kramhome"
)

// Store is a flat env-var-name -> API-key map, persisted as JSON, plus a
// separate map of refreshable OAuth tokens (see OAuthToken) for accounts
// connected via a browser-login subscription flow rather than a pasted
// developer API key.
type Store struct {
	path      string
	keys      map[string]string
	oauthPath string
	oauth     map[string]OAuthToken
}

// OAuthToken is a refreshable credential — a short-lived access token
// plus the refresh token that can mint a new one, as returned by
// Anthropic's and OpenAI's browser-login flows (internal/oauthflow).
// Unlike the plain keys map, these expire and must be refreshed before
// use; see Store.Resolve.
type OAuthToken struct {
	Access    string    `json:"access"`
	Refresh   string    `json:"refresh"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Load reads the credentials file (kramhome.Path("credentials.json")) and
// the OAuth token file (kramhome.Path("oauth_tokens.json")), or leaves
// either empty if it doesn't exist yet — a missing file is the normal
// first-run state, not an error. The two are kept in separate files
// rather than one so the well-established plain-key format and its
// existing callers are never touched by the OAuth addition.
func Load() (*Store, error) {
	path, err := kramhome.Path("credentials.json")
	if err != nil {
		return nil, err
	}
	oauthPath, err := kramhome.Path("oauth_tokens.json")
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, keys: make(map[string]string), oauthPath: oauthPath, oauth: make(map[string]OAuthToken)}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &s.keys); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	oauthData, err := os.ReadFile(oauthPath)
	if err == nil {
		if err := json.Unmarshal(oauthData, &s.oauth); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	return s, nil
}

// Get returns the stored key for envVar, or "" if none is set. This never
// looks at OAuth tokens — a caller that needs a currently-valid credential
// regardless of which kind was stored should use Resolve instead.
func (s *Store) Get(envVar string) string {
	return s.keys[envVar]
}

// All returns every stored env-var -> key pair (plain keys only, not
// OAuth tokens).
func (s *Store) All() map[string]string {
	out := make(map[string]string, len(s.keys))
	for k, v := range s.keys {
		out[k] = v
	}
	return out
}

// Set stores a key for envVar and persists immediately — the accounts
// screen has no separate "save" step, so a key is durable the moment it's
// entered.
func (s *Store) Set(envVar, key string) error {
	s.keys[envVar] = key
	return s.save()
}

// Delete removes a stored key and persists immediately.
func (s *Store) Delete(envVar string) error {
	delete(s.keys, envVar)
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.keys, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// GetOAuth returns the stored OAuth token for envVar and whether one
// exists — note this can be expired; callers that need a currently-valid
// token should use Resolve, which refreshes automatically.
func (s *Store) GetOAuth(envVar string) (OAuthToken, bool) {
	tok, ok := s.oauth[envVar]
	return tok, ok
}

// SetOAuth stores an OAuth token for envVar and persists immediately.
func (s *Store) SetOAuth(envVar string, tok OAuthToken) error {
	s.oauth[envVar] = tok
	return s.saveOAuth()
}

// DeleteOAuth removes a stored OAuth token and persists immediately.
func (s *Store) DeleteOAuth(envVar string) error {
	delete(s.oauth, envVar)
	return s.saveOAuth()
}

func (s *Store) saveOAuth() error {
	if err := os.MkdirAll(filepath.Dir(s.oauthPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.oauth, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.oauthPath, data, 0o600)
}

// refreshSkew is how far ahead of the real expiry Resolve treats a token
// as due for refresh — a small safety margin so a token doesn't expire
// mid-request between Resolve returning it and the caller actually using
// it.
const refreshSkew = 60 * time.Second

// Resolve returns a currently-valid credential for envVar. If a stored
// OAuth token exists and isn't within refreshSkew of expiring, its access
// token is returned as-is; if it's expired or about to be, refresh is
// called with the stored refresh token, the result is persisted (via
// SetOAuth) before returning, and — since some providers don't rotate the
// refresh token on every refresh — an empty Refresh in the result is
// treated as "unchanged" rather than discarding the still-valid one. If
// no OAuth token is stored for envVar at all, this falls back to the
// plain Get(envVar) value, so callers that don't care which kind of
// credential is configured can always call Resolve.
func (s *Store) Resolve(ctx context.Context, envVar string, refresh func(ctx context.Context, refreshToken string) (OAuthToken, error)) (string, error) {
	tok, ok := s.oauth[envVar]
	if !ok {
		if key := s.keys[envVar]; key != "" {
			return key, nil
		}
		return "", fmt.Errorf("no credential stored for %s", envVar)
	}

	if time.Now().Add(refreshSkew).Before(tok.ExpiresAt) {
		return tok.Access, nil
	}

	if refresh == nil {
		return "", fmt.Errorf("credential for %s has expired and cannot be refreshed", envVar)
	}
	fresh, err := refresh(ctx, tok.Refresh)
	if err != nil {
		return "", fmt.Errorf("refreshing credential for %s: %w", envVar, err)
	}
	if fresh.Refresh == "" {
		fresh.Refresh = tok.Refresh
	}
	if err := s.SetOAuth(envVar, fresh); err != nil {
		return "", fmt.Errorf("saving refreshed credential for %s: %w", envVar, err)
	}
	return fresh.Access, nil
}
