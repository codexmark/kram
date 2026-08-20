package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/kramhome"
)

// skillInstallTimeout bounds the clone — a public repo that isn't
// responding shouldn't park an agent turn indefinitely.
const skillInstallTimeout = 90 * time.Second

type skillInstall struct {
	clone func(context.Context, string, string) ([]byte, error)
}

func newSkillInstall() *skillInstall {
	return &skillInstall{clone: cloneSkillRepository}
}

func (t *skillInstall) Name() string { return "skill_install" }
func (t *skillInstall) Description() string {
	return "Install skills from a public git repository into the global skill directory. Clones the repo, finds every folder containing a SKILL.md, and installs the ones requested (or lists what's available if none are named). Always check the repository's license before installing — the tool records the source but cannot judge whether copying is permitted."
}

func (t *skillInstall) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repo": {"type": "string", "description": "Git URL of a public repository, e.g. https://github.com/owner/name"},
			"skills": {"type": "array", "items": {"type": "string"}, "description": "Names of skills to install. Omit to list what the repo contains without installing anything."}
		},
		"required": ["repo"]
	}`)
}

type skillInstallArgs struct {
	Repo   string   `json:"repo"`
	Skills []string `json:"skills"`
}

// cloneSkillRepository is a narrow test seam around the only external
// process in skill installation. Production always uses git with the same
// shallow-clone arguments; tests can substitute a deterministic local copy
// without standing up a smart-HTTP Git server.
func cloneSkillRepository(ctx context.Context, repo, dst string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--quiet", repo, dst)
	return cmd.CombinedOutput()
}

func (t *skillInstall) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args skillInstallArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.Repo == "" {
		return "error: repo must not be empty", nil
	}
	if !strings.HasPrefix(args.Repo, "https://") && !strings.HasPrefix(args.Repo, "http://") {
		// git:// and ssh remotes would need credentials and can reach
		// private hosts; skills are meant to come from public repos.
		return "error: repo must be an http(s) git URL", nil
	}

	tmp, err := os.MkdirTemp("", "kram-skill-install-")
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	defer os.RemoveAll(tmp)

	cloneCtx, cancel := context.WithTimeout(ctx, skillInstallTimeout)
	defer cancel()
	if out, err := t.clone(cloneCtx, args.Repo, tmp); err != nil {
		return fmt.Sprintf("error: cloning %s failed: %v\n%s", args.Repo, err, strings.TrimSpace(string(out))), nil
	}

	found, err := findSkillDirs(tmp)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(found) == 0 {
		return fmt.Sprintf("no SKILL.md files found in %s", args.Repo), nil
	}

	if len(args.Skills) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d skills available in %s (none installed — pass \"skills\" to choose):\n", len(found), args.Repo)
		names := make([]string, 0, len(found))
		for name := range found {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, n := range names {
			b.WriteString("- " + n + "\n")
		}
		if lic := detectLicense(tmp); lic != "" {
			fmt.Fprintf(&b, "\nRepository license file says: %s\n", lic)
		} else {
			b.WriteString("\nWarning: no LICENSE file found in this repository — copying may not be permitted.\n")
		}
		return b.String(), nil
	}

	dest, err := kramhome.Path("skills")
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	var installed, missing []string
	for _, want := range args.Skills {
		src, ok := found[want]
		if !ok {
			missing = append(missing, want)
			continue
		}
		if err := installSkill(src, filepath.Join(dest, want), args.Repo); err != nil {
			return fmt.Sprintf("error: installing %s: %v", want, err), nil
		}
		installed = append(installed, want)
	}

	var b strings.Builder
	if len(installed) > 0 {
		fmt.Fprintf(&b, "installed %d skill(s) from %s: %s\n", len(installed), args.Repo, strings.Join(installed, ", "))
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "not found in that repo: %s\n", strings.Join(missing, ", "))
	}
	return b.String(), nil
}

// findSkillDirs maps skill name -> directory for every folder holding a
// SKILL.md. The name is the containing directory's name, which is the
// convention every published collection follows.
func findSkillDirs(root string) (map[string]string, error) {
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip rather than fail the whole scan
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "SKILL.md" {
			dir := filepath.Dir(path)
			out[filepath.Base(dir)] = dir
		}
		return nil
	})
	return out, err
}

// installSkill copies one skill directory, recording where it came from
// so provenance survives in the installed copy — the same reason a
// vendored dependency keeps its origin.
func installSkill(src, dst, repo string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // one level only: SKILL.md plus its sibling reference files
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

// detectLicense returns the first line of a root LICENSE file, which is
// enough to tell MIT from Apache from "all rights reserved" at a glance.
// Deliberately reported, never enforced: judging whether a license
// permits copying is the user's call, not a regex's.
func detectLicense(root string) string {
	for _, name := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return ""
}
