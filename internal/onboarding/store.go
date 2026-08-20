// Package onboarding tracks whether the first-run setup wizard has been
// completed — a small, versioned marker so a future wizard redesign can
// force existing installs through it again (bump currentVersion), while a
// completed install on the current version never sees it uninvited.
package onboarding

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/codexmark/kram/internal/kramhome"
)

// currentVersion is compared against a saved State's Version to decide
// whether the wizard needs to run again after a schema/step change —
// bump it whenever a new required step is added. Deliberately not
// migration logic (see DECISIONS.md): a version bump just re-triggers the
// wizard from scratch, it doesn't try to patch an old State forward.
const currentVersion = 1

// State is what persists across runs about the first-run wizard.
// ProjectsRoot and LastWorkspace are seed data for a future "projects
// root + picker" launcher (see DECISIONS.md) — Kram doesn't read them
// for anything today beyond pre-filling the wizard's own defaults.
type State struct {
	Version       int    `json:"version"`
	Completed     bool   `json:"completed"`
	ProjectsRoot  string `json:"projects_root,omitempty"`
	LastWorkspace string `json:"last_workspace,omitempty"`
}

// NeedsSetup reports whether the wizard should run: never completed, or
// completed against an older wizard version than this build knows about.
func (s State) NeedsSetup() bool {
	return !s.Completed || s.Version < currentVersion
}

// Load reads the onboarding state file (kramhome.Path("onboarding.json")),
// or returns a zero State (NeedsSetup() == true) if it doesn't exist yet —
// a missing file is the normal first-run state, not an error.
func Load() (State, error) {
	path, err := kramhome.Path("onboarding.json")
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, err
	}
	return s, nil
}

// SaveProgress persists the choices needed to resume/re-run setup while
// deliberately leaving Completed false. Stage 1 uses this before the daemon
// starts; if the user exits during Stage 2, the next launch therefore opens
// setup again instead of mistaking a half-finished wizard for a completed one.
func SaveProgress(s State) error {
	s.Completed = false
	return save(s)
}

// Save persists a fully completed wizard state. It is only called after the
// user confirms the final Ready step, never merely because Stage 1 produced a
// usable gateway configuration.
func Save(s State) error {
	s.Completed = true
	return save(s)
}

func save(s State) error {
	path, err := kramhome.Path("onboarding.json")
	if err != nil {
		return err
	}
	s.Version = currentVersion
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
