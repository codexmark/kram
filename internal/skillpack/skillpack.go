// Package skillpack installs a skill repository's contents into the
// global skill directory — the onboarding wizard's "starter pack" path
// (#135). It intentionally mirrors the daemon tool skill_install's
// behavior (clone, find <name>/SKILL.md dirs, copy one level, record a
// SOURCE file) without importing internal/daemon/tools: the wizard runs
// in the CLI process, and pulling the daemon's tool layer into the CLI
// for one helper would invert the layering — the same small-duplication
// trade the codebase already documents for oauthRefreshAdapter.
package skillpack

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/codexmark/kram/internal/kramhome"
)

// DefaultRepo is the starter pack the wizard offers: the curated
// kram-skills collection.
const DefaultRepo = "https://github.com/codexmark/kram-skills"

// Install clones repo (shallow) and installs every skill it contains
// (each directory holding a SKILL.md) into the global skill directory,
// recording repo in each skill's SOURCE file. Returns the installed
// names, sorted. Network and git failures return an error — the caller
// decides whether that blocks anything (the wizard never lets it).
func Install(ctx context.Context, repo string) ([]string, error) {
	tmp, err := os.MkdirTemp("", "kram-skillpack-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	clone := exec.CommandContext(cctx, "git", "clone", "--depth", "1", "--quiet", repo, tmp)
	if out, err := clone.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("cloning %s: %v (%s)", repo, err, string(out))
	}

	dest, err := kramhome.Path("skills")
	if err != nil {
		return nil, err
	}

	var installed []string
	err = filepath.WalkDir(tmp, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || d.Name() == ".git" {
			if d != nil && d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return err
		}
		if _, statErr := os.Stat(filepath.Join(path, "SKILL.md")); statErr != nil {
			return nil
		}
		name := filepath.Base(path)
		if err := copySkillDir(path, filepath.Join(dest, name), repo); err != nil {
			return fmt.Errorf("installing %s: %w", name, err)
		}
		installed = append(installed, name)
		return filepath.SkipDir // one skill per directory; don't descend further
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(installed)
	return installed, nil
}

// copySkillDir copies one level of files (SKILL.md plus sibling
// references) and records the source repo — the same shape the
// skill_install tool produces, so both paths are indistinguishable on
// disk.
func copySkillDir(src, dst, repo string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dst, "SOURCE"), []byte(repo+"\n"), 0o644)
}
