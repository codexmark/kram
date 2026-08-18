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
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/codexmark/kram-gateway/internal/kramhome"
)

// Store is a flat env-var-name -> API-key map, persisted as JSON.
type Store struct {
	path string
	keys map[string]string
}

// Load reads the credentials file (kramhome.Path("credentials.json")), or
// returns an empty Store if it doesn't exist yet — a missing file is the
// normal first-run state, not an error.
func Load() (*Store, error) {
	path, err := kramhome.Path("credentials.json")
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, keys: make(map[string]string)}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.keys); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns the stored key for envVar, or "" if none is set.
func (s *Store) Get(envVar string) string {
	return s.keys[envVar]
}

// All returns every stored env-var -> key pair.
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
