// Package kramhome resolves Kram's own local config directory — the one
// shared root that credentials, tool/skill settings, and global skills all
// live under, so there's a single place this decision is made instead of
// each package inventing its own path.
package kramhome

import (
	"os"
	"path/filepath"
)

// Dir returns $XDG_CONFIG_HOME/kram-gateway, falling back to
// ~/.config/kram-gateway. Deliberately "kram-gateway" (this repo's actual
// name), not the shorter "kram" — verified against a real machine during
// development that a plain "kram" directory under ~/.config was already
// in use by a completely unrelated tool (an npm/bun-based project also
// called "kram"), so the shorter name risks colliding with someone else's
// config on the very first write.
func Dir() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "kram-gateway"), nil
}

// Path joins Dir() with the given path elements — e.g.
// kramhome.Path("credentials.json") or kramhome.Path("skills").
func Path(elem ...string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{dir}, elem...)...), nil
}
