package tools

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSnapshotToolsRegisteredAndReachable is a light integration check,
// distinct from internal/snapshot's own unit tests: it confirms the four
// snapshot_* tools are actually wired into the Registry (not just
// compiling in isolation) and that a full create -> diff -> restore
// round trip works through Registry.Execute exactly as the model would
// call it, including the permission/policy path every tool call goes
// through.
func TestSnapshotToolsRegisteredAndReachable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()

	userGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("user git %v: %v: %s", args, err, out.String())
		}
		return out.String()
	}
	userGit("init", "--quiet")
	userGit("config", "user.email", "user@example.com")
	userGit("config", "user.name", "Test User")
	writeWorkspaceFile(t, workspace, "app.go", "package main\n")
	userGit("add", "app.go")
	userGit("commit", "--quiet", "-m", "initial")
	// .kram already exists in any real Kram-managed workspace before a
	// snapshot is ever taken (session database, artifacts, ...) —
	// pre-creating it isolates this assertion to what the snapshot tools
	// themselves do to the user's git state, not the unrelated fact that
	// .kram's directory didn't exist on disk yet.
	writeWorkspaceFile(t, workspace, ".kram/placeholder", "")
	statusBefore := userGit("status", "--short")

	r := NewRegistry(workspace, nil, nil)
	ctx := context.Background()

	// snapshot_list on a fresh workspace.
	out, err := r.Execute(ctx, "snapshot_list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no snapshots yet") {
		t.Errorf("snapshot_list on empty store = %q", out)
	}

	// snapshot_create.
	out, err = r.Execute(ctx, "snapshot_create", mustJSON(t, map[string]string{"message": "before change"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "snapshot created") {
		t.Fatalf("snapshot_create = %q", out)
	}
	id := strings.Fields(strings.TrimPrefix(out, "snapshot created: "))[0]

	// Mutate the workspace.
	writeWorkspaceFile(t, workspace, "app.go", "package main\n\nfunc main() {}\n")

	// snapshot_diff should show the change without applying it.
	out, err = r.Execute(ctx, "snapshot_diff", mustJSON(t, map[string]string{"id": id}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "app.go") {
		t.Errorf("snapshot_diff = %q, expected it to mention app.go", out)
	}
	if got := readWorkspaceFile(t, workspace, "app.go"); !strings.Contains(got, "func main()") {
		t.Fatalf("snapshot_diff mutated the workspace: %q", got)
	}

	// snapshot_restore requires an explicit id.
	out, err = r.Execute(ctx, "snapshot_restore", mustJSON(t, map[string]string{"id": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("snapshot_restore with empty id = %q, expected an error", out)
	}

	// snapshot_restore with the real id applies the change back.
	out, err = r.Execute(ctx, "snapshot_restore", mustJSON(t, map[string]string{"id": id}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "app.go") {
		t.Errorf("snapshot_restore = %q, expected it to report app.go changed", out)
	}
	if got := readWorkspaceFile(t, workspace, "app.go"); got != "package main\n" {
		t.Errorf("app.go = %q, want the snapshotted content", got)
	}

	// The user's real git state must be exactly as it was.
	if statusAfter := userGit("status", "--short"); statusAfter != statusBefore {
		t.Errorf("user git status changed:\nbefore: %q\nafter:  %q", statusBefore, statusAfter)
	}

	// snapshot_list now shows the one snapshot taken.
	out, err = r.Execute(ctx, "snapshot_list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "before change") {
		t.Errorf("snapshot_list = %q, expected it to include the snapshot's message", out)
	}
}

// TestSnapshotRestoreDescriptionIsExplicitlyDestructive guards against
// the description drifting away from clearly warning the model (and
// anyone reading the tool list) that this call overwrites files.
func TestSnapshotRestoreDescriptionIsExplicitlyDestructive(t *testing.T) {
	desc := newSnapshotRestore(nil).Description()
	if !strings.Contains(desc, "DESTRUCTIVE") {
		t.Errorf("snapshot_restore description should call out that it's destructive, got: %q", desc)
	}
}

func writeWorkspaceFile(t *testing.T, workspace, rel, content string) {
	t.Helper()
	path := filepath.Join(workspace, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWorkspaceFile(t *testing.T, workspace, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
