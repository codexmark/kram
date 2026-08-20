// Package toolsettings is a small local store of which tools and skills
// are disabled — the on/off switch behind the CLI's tools/skills screen.
// Same on-disk shape and guarantees as internal/credentials: plain JSON
// under kramhome, 0600, no separate save step.
package toolsettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/codexmark/kram/internal/kramhome"
)

// Store is a set of disabled item names. Tools and skills share one
// namespace here — a tool name like "bash" and a skill name never
// legitimately collide since skills are user-authored and tool names are
// a fixed built-in set, so there's no need to prefix/separate them.
type Store struct {
	path     string
	disabled map[string]bool
}

// Load reads the settings file, or returns an empty (everything enabled)
// Store if it doesn't exist yet.
func Load() (*Store, error) {
	path, err := kramhome.Path("tool-settings.json")
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, disabled: make(map[string]bool)}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, err
	}
	for _, n := range names {
		s.disabled[n] = true
	}
	return s, nil
}

// IsDisabled reports whether name is turned off.
func (s *Store) IsDisabled(name string) bool {
	return s.disabled[name]
}

// Disabled returns every disabled name.
func (s *Store) Disabled() map[string]bool {
	out := make(map[string]bool, len(s.disabled))
	for k := range s.disabled {
		out[k] = true
	}
	return out
}

// SetDisabled turns name on or off and persists immediately.
func (s *Store) SetDisabled(name string, disabled bool) error {
	if disabled {
		s.disabled[name] = true
	} else {
		delete(s.disabled, name)
	}
	return s.save()
}

// SetAllDisabled turns every name in names on or off in one persisted
// write — the tools/skills screen's bulk "enable all"/"disable all"
// keys, so toggling a whole section doesn't fire a save per item.
func (s *Store) SetAllDisabled(names []string, disabled bool) error {
	for _, n := range names {
		if disabled {
			s.disabled[n] = true
		} else {
			delete(s.disabled, n)
		}
	}
	return s.save()
}

// ReplaceDisabled atomically reconciles the store to exactly names. Presets
// use this instead of incremental toggles so applying Recommended or Minimal
// has the same result regardless of whatever a previous setup run disabled.
func (s *Store) ReplaceDisabled(names []string) error {
	next := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			next[n] = true
		}
	}
	s.disabled = next
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	names := make([]string, 0, len(s.disabled))
	for n := range s.disabled {
		names = append(names, n)
	}
	sort.Strings(names)
	data, err := json.MarshalIndent(names, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}
