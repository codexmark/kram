package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortIDAndParseLogLineEdges(t *testing.T) {
	if got := (Snapshot{ID: "1234567890abcdef"}).ShortID(); got != "1234567890ab" {
		t.Fatalf("short=%q", got)
	}
	if got := (Snapshot{ID: "short"}).ShortID(); got != "short" {
		t.Fatalf("short=%q", got)
	}
	if _, err := parseLogLine("bad"); err == nil {
		t.Fatal("malformed log accepted")
	}
	s, err := parseLogLine("abc\x1fnot-a-time\x1fmessage")
	if err != nil || !s.CreatedAt.IsZero() || s.Message != "message" {
		t.Fatalf("snapshot=%#v err=%v", s, err)
	}
}

func TestEmptyStoreNoDiffAndInvalidIDs(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir())
	snaps, err := store.List(ctx)
	if err != nil || snaps != nil {
		t.Fatalf("list=%#v err=%v", snaps, err)
	}
	if _, err := store.Diff(ctx, "--evil"); err == nil || !strings.Contains(err.Error(), "invalid snapshot id") {
		t.Fatalf("err=%v", err)
	}
	if _, err := store.Restore(ctx, "abcdef0"); err == nil || !strings.Contains(err.Error(), "no such snapshot") {
		t.Fatalf("err=%v", err)
	}
	if err := os.WriteFile(filepath.Join(store.workspace, "file.txt"), []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Message == "" || !strings.HasPrefix(snap.Message, "snapshot ") {
		t.Fatalf("message=%q", snap.Message)
	}
	diff, err := store.Diff(ctx, snap.ID)
	if err != nil || !strings.Contains(diff, "no differences") {
		t.Fatalf("diff=%q err=%v", diff, err)
	}
}

func TestChangesForAllStatuses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := NewStore(root)
	for name, content := range map[string]string{"modified": "old", "deleted": "restore me"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := store.Create(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "modified"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "added"), []byte("remove me"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A later snapshot teaches the isolated index about the newly-added path;
	// previewing the older snapshot can then classify it as a removal.
	if _, err := store.Create(ctx, "later"); err != nil {
		t.Fatal(err)
	}
	changes, err := store.changesFor(ctx, snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, c := range changes {
		statuses[c.Path] = c.Status
	}
	if statuses["modified"] != "will be overwritten" || statuses["deleted"] != "will be restored" || statuses["added"] != "will be removed" {
		t.Fatalf("changes=%#v", changes)
	}
}
