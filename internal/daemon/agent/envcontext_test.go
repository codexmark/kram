package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCollectEnvContextShapes(t *testing.T) {
	ctx := context.Background()

	// Non-repo: date + combo only, no Git lines, no lies.
	plain := t.TempDir()
	out := collectEnvContext(ctx, plain, "default")
	if !strings.Contains(out, "Today's date:") || !strings.Contains(out, "Active model combo: default") || strings.Contains(out, "Git:") {
		t.Fatalf("non-repo env = %q", out)
	}

	// Empty workspace: no git probing at all.
	if out := collectEnvContext(ctx, "", "m"); strings.Contains(out, "Git:") {
		t.Fatalf("empty workspace must not probe git: %q", out)
	}

	// Unborn repo (git init, no commits): branch reported, no commits line.
	unborn := t.TempDir()
	gitIn(t, unborn, "init", "-b", "main")
	out = collectEnvContext(ctx, unborn, "")
	if !strings.Contains(out, "Git: branch main") || strings.Contains(out, "Recent commits") {
		t.Fatalf("unborn repo env = %q", out)
	}

	// Real repo, dirty tree, detached HEAD.
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-m", "first")
	out = collectEnvContext(ctx, repo, "")
	if !strings.Contains(out, "Git: branch main, working tree clean") || !strings.Contains(out, "first") {
		t.Fatalf("clean repo env = %q", out)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = collectEnvContext(ctx, repo, "")
	if !strings.Contains(out, "1 file(s) modified or untracked") {
		t.Fatalf("dirty repo env = %q", out)
	}
	gitIn(t, repo, "checkout", "--detach", "HEAD")
	out = collectEnvContext(ctx, repo, "")
	if !strings.Contains(out, "detached HEAD at ") || strings.Contains(out, "branch HEAD") {
		t.Fatalf("detached env = %q", out)
	}
}
