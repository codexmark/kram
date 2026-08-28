package skillpack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallFromLocalRepo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A local git repo works as a clone URL — the same code path as a
	// remote, minus the network.
	repo := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(repo, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: d\n---\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a non-skill dir must be ignored
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("add", ".")
	run("commit", "-m", "skills")

	installed, err := Install(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 2 || installed[0] != "alpha" || installed[1] != "beta" {
		t.Fatalf("installed = %v", installed)
	}

	skillsDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kram-gateway", "skills")
	data, err := os.ReadFile(filepath.Join(skillsDir, "alpha", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "name: alpha") {
		t.Fatalf("alpha SKILL.md = %q err=%v", data, err)
	}
	source, err := os.ReadFile(filepath.Join(skillsDir, "alpha", "SOURCE"))
	if err != nil || strings.TrimSpace(string(source)) != repo {
		t.Fatalf("SOURCE = %q err=%v", source, err)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "docs")); !os.IsNotExist(err) {
		t.Fatal("non-skill directory must not be installed")
	}
}

func TestInstallBadRepoErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := Install(context.Background(), filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected clone error")
	}
}

// TestInstallCloneFailure: an unclonable repo surfaces as a real error —
// the caller (the wizard) decides what that blocks, never this package.
func TestInstallCloneFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := Install(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil || !strings.Contains(err.Error(), "cloning") {
		t.Fatalf("err = %v, want a cloning error", err)
	}
}

// TestInstallCopiesSiblingsSkipsNestedDirs: a skill directory's sibling
// files (references next to SKILL.md) install alongside it, nested
// directories are not descended into, and SOURCE records the repo —
// the same on-disk shape skill_install produces.
func TestInstallCopiesSiblingsSkipsNestedDirs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repo := t.TempDir()
	dir := filepath.Join(repo, "gamma")
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"SKILL.md":     "---\nname: gamma\ndescription: d\n---\nbody",
		"reference.md": "extra notes",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "deep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("add", ".")
	run("commit", "-m", "skills")

	installed, err := Install(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0] != "gamma" {
		t.Fatalf("installed = %v", installed)
	}
	dst := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kram-gateway", "skills", "gamma")
	for _, want := range []string{"SKILL.md", "reference.md", "SOURCE"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Fatalf("%s missing after install: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "assets")); !os.IsNotExist(err) {
		t.Fatalf("nested directory must not be copied, stat err = %v", err)
	}
	source, err := os.ReadFile(filepath.Join(dst, "SOURCE"))
	if err != nil || strings.TrimSpace(string(source)) != repo {
		t.Fatalf("SOURCE = %q err=%v, want the repo path", source, err)
	}
}

// TestCopySkillDirErrors pins the two error paths reachable without
// root-only permission tricks: an unreadable source directory, and a
// destination whose parent is a plain file (MkdirAll refuses).
func TestCopySkillDirErrors(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copySkillDir(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "dst"), "r"); err == nil {
		t.Fatal("want an error for an unreadable source")
	}
	parentFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parentFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copySkillDir(src, filepath.Join(parentFile, "dst"), "r"); err == nil {
		t.Fatal("want an error when the destination parent is a file")
	}
}
