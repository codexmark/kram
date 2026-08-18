package snapshot

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newUserRepo creates a real git repository at workspace — standing in
// for "the user's actual project" — with one committed file, and returns
// a helper to run git against it directly (for assertions, never for the
// snapshot code itself, which must never touch this repo's --git-dir).
func newUserRepo(t *testing.T) (workspace string, userGit func(args ...string) string) {
	t.Helper()
	workspace = t.TempDir()
	userGit = func(args ...string) string {
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
	writeFile(t, workspace, "tracked.txt", "v1")
	userGit("add", "tracked.txt")
	userGit("commit", "--quiet", "-m", "initial")
	return workspace, userGit
}

func writeFile(t *testing.T, workspace, rel, content string) {
	t.Helper()
	path := filepath.Join(workspace, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, workspace, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}

func exists(workspace, rel string) bool {
	_, err := os.Stat(filepath.Join(workspace, rel))
	return err == nil
}

// TestCreateAndRestoreModifiedFile covers the core round trip: a file
// tracked by the user's own git gets modified twice; a snapshot taken
// between the two edits should let Restore bring back the middle state.
func TestCreateAndRestoreModifiedFile(t *testing.T) {
	workspace, _ := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	writeFile(t, workspace, "tracked.txt", "v2-at-snapshot-time")
	snap, err := s.Create(ctx, "before risky edit")
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, workspace, "tracked.txt", "v3-oops")

	result, err := s.Restore(ctx, snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, workspace, "tracked.txt"); got != "v2-at-snapshot-time" {
		t.Errorf("tracked.txt = %q, want %q", got, "v2-at-snapshot-time")
	}
	if len(result.Changes) == 0 {
		t.Error("expected Restore to report at least one changed file")
	}
}

// TestCreateCapturesUntrackedFile covers acceptance criterion 3: a file
// that was never `git add`ed by the user must still be captured.
func TestCreateCapturesUntrackedFile(t *testing.T) {
	workspace, userGit := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	writeFile(t, workspace, "scratch.txt", "hello untracked")
	if status := userGit("status", "--short"); !strings.Contains(status, "?? scratch.txt") {
		t.Fatalf("precondition failed: scratch.txt should be untracked, got status: %q", status)
	}

	snap, err := s.Create(ctx, "capture untracked")
	if err != nil {
		t.Fatal(err)
	}

	os.Remove(filepath.Join(workspace, "scratch.txt"))
	if _, err := s.Restore(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if !exists(workspace, "scratch.txt") {
		t.Fatal("expected scratch.txt to be recreated by restore")
	}
	if got := readFile(t, workspace, "scratch.txt"); got != "hello untracked" {
		t.Errorf("scratch.txt = %q, want %q", got, "hello untracked")
	}
}

// TestCreateCapturesDeletedFile covers acceptance criterion 4: a file
// the user deleted before snapshotting must be captured as an absence,
// not error out, and restoring an earlier snapshot (before the delete)
// must bring it back.
func TestCreateCapturesDeletedFile(t *testing.T) {
	workspace, _ := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	before, err := s.Create(ctx, "before delete")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(workspace, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := s.Create(ctx, "after delete")
	if err != nil {
		t.Fatal(err)
	}

	// The file gets recreated by accident, and that recreation is itself
	// captured by a third snapshot — this is what makes the file's
	// presence something Restore can later know to undo. (Restoring an
	// older snapshot only ever undoes what some *later* snapshot also
	// captured — a file resurrected without ever being snapshotted again
	// is, by design, invisible to Restore; see
	// TestRestoreLeavesNeverSnapshottedFileAlone.)
	writeFile(t, workspace, "tracked.txt", "resurrected by accident")
	if _, err := s.Create(ctx, "resurrected"); err != nil {
		t.Fatal(err)
	}

	// Restoring the post-delete snapshot must remove it again.
	if _, err := s.Restore(ctx, afterDelete.ID); err != nil {
		t.Fatal(err)
	}
	if exists(workspace, "tracked.txt") {
		t.Error("expected tracked.txt to be removed, restoring the post-delete snapshot")
	}

	// Restoring the pre-delete snapshot must bring it back.
	if _, err := s.Restore(ctx, before.ID); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, workspace, "tracked.txt"); got != "v1" {
		t.Errorf("tracked.txt = %q, want %q", got, "v1")
	}
}

// TestSnapshotNeverTouchesUserGitState is the explicit test the spec
// calls for: the user's real git index, staged content, current branch
// and HEAD must be byte-identical before and after a Create and a
// Restore.
func TestSnapshotNeverTouchesUserGitState(t *testing.T) {
	workspace, userGit := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	// Stage a change the user hasn't committed — this must survive
	// completely untouched by everything the snapshot package does.
	writeFile(t, workspace, "staged.txt", "staged content")
	userGit("add", "staged.txt")

	// .kram already exists in any real Kram-managed workspace (the
	// session database, artifacts, etc. all live there) before a
	// snapshot is ever taken. Pre-creating it here isolates the
	// assertion below to what Create/Restore themselves do to the
	// user's git state, rather than to the unrelated fact that .kram's
	// directory didn't exist on disk yet.
	writeFile(t, workspace, ".kram/placeholder", "")

	branchBefore := strings.TrimSpace(userGit("rev-parse", "--abbrev-ref", "HEAD"))
	headBefore := strings.TrimSpace(userGit("rev-parse", "HEAD"))
	indexBefore := readUserIndexBlob(t, workspace, userGit, "staged.txt")
	statusBefore := userGit("status", "--short")

	snap, err := s.Create(ctx, "with staged change present")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, "tracked.txt", "mutated after snapshot")
	if _, err := s.Restore(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}

	branchAfter := strings.TrimSpace(userGit("rev-parse", "--abbrev-ref", "HEAD"))
	headAfter := strings.TrimSpace(userGit("rev-parse", "HEAD"))
	indexAfter := readUserIndexBlob(t, workspace, userGit, "staged.txt")
	statusAfter := userGit("status", "--short")

	if branchBefore != branchAfter {
		t.Errorf("branch changed: %q -> %q", branchBefore, branchAfter)
	}
	if headBefore != headAfter {
		t.Errorf("HEAD changed: %q -> %q", headBefore, headAfter)
	}
	if indexBefore != indexAfter {
		t.Errorf("staged blob for staged.txt changed: %q -> %q", indexBefore, indexAfter)
	}
	if statusBefore != statusAfter {
		t.Errorf("git status changed:\nbefore: %q\nafter:  %q", statusBefore, statusAfter)
	}
}

func readUserIndexBlob(t *testing.T, workspace string, userGit func(args ...string) string, path string) string {
	t.Helper()
	out := userGit("ls-files", "--stage", "--", path)
	return strings.TrimSpace(out)
}

// TestDiffDoesNotMutateWorkspace covers acceptance criterion 8: Diff
// must show what a restore would change without applying anything.
func TestDiffDoesNotMutateWorkspace(t *testing.T) {
	workspace, _ := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	snap, err := s.Create(ctx, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, "tracked.txt", "changed after snapshot")

	diff, err := s.Diff(ctx, snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "tracked.txt") {
		t.Errorf("expected diff to mention tracked.txt, got: %q", diff)
	}
	if !strings.Contains(diff, "changed after snapshot") {
		t.Errorf("expected diff to show the new content, got: %q", diff)
	}

	// Nothing was applied — the file on disk must be exactly what it was
	// right before Diff was called.
	if got := readFile(t, workspace, "tracked.txt"); got != "changed after snapshot" {
		t.Errorf("Diff mutated the workspace: tracked.txt = %q", got)
	}
}

// TestRestoreOverStaleWorkspaceOverwritesAndReports exercises the chosen
// "overwrite and report" behavior for restoring an older snapshot after
// the workspace has moved on further: it applies without refusing, and
// the returned RestoreResult names every affected path — see
// DECISIONS.md.
func TestRestoreOverStaleWorkspaceOverwritesAndReports(t *testing.T) {
	workspace, _ := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	old, err := s.Create(ctx, "old snapshot")
	if err != nil {
		t.Fatal(err)
	}

	// Workspace moves on: a second snapshot, then further, unsnapshotted
	// changes — "old" is now stale relative to the live workspace.
	writeFile(t, workspace, "tracked.txt", "v2")
	writeFile(t, workspace, "added-later.txt", "added after old snapshot")
	if _, err := s.Create(ctx, "newer snapshot"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, "tracked.txt", "v3 unsnapshotted")

	result, err := s.Restore(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 {
		t.Fatal("expected the restore over a stale workspace to report changes")
	}
	foundTracked, foundAddedLater := false, false
	for _, c := range result.Changes {
		if c.Path == "tracked.txt" {
			foundTracked = true
		}
		if c.Path == "added-later.txt" {
			foundAddedLater = true
		}
	}
	if !foundTracked {
		t.Errorf("expected tracked.txt in reported changes, got %+v", result.Changes)
	}
	if !foundAddedLater {
		t.Errorf("expected added-later.txt in reported changes, got %+v", result.Changes)
	}

	if got := readFile(t, workspace, "tracked.txt"); got != "v1" {
		t.Errorf("tracked.txt = %q, want %q (the state at the old snapshot)", got, "v1")
	}
	if exists(workspace, "added-later.txt") {
		t.Error("expected added-later.txt to be removed, restoring a snapshot from before it existed")
	}
}

// TestRestoreLeavesNeverSnapshottedFileAlone documents and tests the
// other half of that same decision: a file the snapshot system has no
// record of at all (created after the most recent snapshot, never
// captured by any Create call) is left untouched by Restore — the
// package only ever undoes what it once knew about, never performs a
// blanket "clean the workspace" pass.
func TestRestoreLeavesNeverSnapshottedFileAlone(t *testing.T) {
	workspace, _ := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	snap, err := s.Create(ctx, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, workspace, "never-snapshotted.txt", "nobody has ever seen this")

	if _, err := s.Restore(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if !exists(workspace, "never-snapshotted.txt") {
		t.Error("expected a file with no snapshot history to survive Restore untouched")
	}
}

// TestKramDirExcludedFromSnapshot covers acceptance criterion 6.
func TestKramDirExcludedFromSnapshot(t *testing.T) {
	workspace, _ := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	writeFile(t, workspace, ".kram/sessions.db", "pretend-sqlite-bytes")
	writeFile(t, workspace, ".kram/other/nested.json", "{}")

	snap, err := s.Create(ctx, "with .kram present")
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("git", "--git-dir="+filepath.Join(workspace, ".kram", "snapshots", ".git"),
		"ls-tree", "-r", "--name-only", snap.ID).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v: %s", err, out)
	}
	if strings.Contains(string(out), ".kram") {
		t.Errorf("snapshot tree must never contain .kram, got:\n%s", out)
	}
}

// TestGitignoredFileNeverCaptured documents the chosen .gitignore
// behavior: patterns in the user's own .gitignore are respected, since
// the isolated repo's --work-tree is the same directory those
// .gitignore files live in.
func TestGitignoredFileNeverCaptured(t *testing.T) {
	workspace, userGit := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	writeFile(t, workspace, ".gitignore", "ignored.txt\n")
	userGit("add", ".gitignore")
	userGit("commit", "--quiet", "-m", "add gitignore")
	writeFile(t, workspace, "ignored.txt", "should never be snapshotted")

	snap, err := s.Create(ctx, "with an ignored file present")
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("git", "--git-dir="+filepath.Join(workspace, ".kram", "snapshots", ".git"),
		"ls-tree", "-r", "--name-only", snap.ID).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v: %s", err, out)
	}
	if strings.Contains(string(out), "ignored.txt") {
		t.Errorf("snapshot tree must not contain a .gitignore'd file, got:\n%s", out)
	}
}

// TestUnavailableWithoutGit covers the graceful-degradation requirement:
// with git missing from PATH, every Store method must return
// ErrUnavailable instead of panicking or hanging.
func TestUnavailableWithoutGit(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("PATH", t.TempDir()) // an empty directory: no git found

	s := NewStore(workspace)
	ctx := context.Background()

	if err := Available(); err != ErrUnavailable {
		t.Errorf("Available() = %v, want ErrUnavailable", err)
	}
	if _, err := s.Create(ctx, "x"); err != ErrUnavailable {
		t.Errorf("Create() error = %v, want ErrUnavailable", err)
	}
	if _, err := s.List(ctx); err != ErrUnavailable {
		t.Errorf("List() error = %v, want ErrUnavailable", err)
	}
	if _, err := s.Diff(ctx, "deadbeef"); err != ErrUnavailable {
		t.Errorf("Diff() error = %v, want ErrUnavailable", err)
	}
	if _, err := s.Restore(ctx, "deadbeef"); err != ErrUnavailable {
		t.Errorf("Restore() error = %v, want ErrUnavailable", err)
	}
}

// TestListReturnsSnapshotsNewestFirst covers the basic listing contract
// the snapshot_list tool depends on.
func TestListReturnsSnapshotsNewestFirst(t *testing.T) {
	workspace, _ := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	if snaps, err := s.List(ctx); err != nil || len(snaps) != 0 {
		t.Fatalf("List() on a fresh workspace = %+v, %v; want empty, nil", snaps, err)
	}

	first, err := s.Create(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}

	snaps, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("List() returned %d snapshots, want 2", len(snaps))
	}
	if snaps[0].ID != second.ID || snaps[1].ID != first.ID {
		t.Errorf("List() order = [%s, %s], want newest first", snaps[0].ID, snaps[1].ID)
	}
}

// TestResolveRejectsMalformedID guards the same class of bug
// artifact.Store.paths exists to prevent: an id must look like a real
// hash before it's ever handed to git as a revision argument.
func TestResolveRejectsMalformedID(t *testing.T) {
	workspace, _ := newUserRepo(t)
	ctx := context.Background()
	s := NewStore(workspace)

	if _, err := s.Create(ctx, "baseline"); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "-not-a-hash", "../../etc/passwd", "HEAD~1", "not hex zz"} {
		if _, err := s.Diff(ctx, bad); err == nil {
			t.Errorf("Diff(%q) succeeded, want an error", bad)
		}
		if _, err := s.Restore(ctx, bad); err == nil {
			t.Errorf("Restore(%q) succeeded, want an error", bad)
		}
	}
}
