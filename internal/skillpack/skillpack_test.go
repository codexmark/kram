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
