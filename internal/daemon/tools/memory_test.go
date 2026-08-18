package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codexmark/kram-gateway/internal/daemon/store"
)

func newMemoryTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMemoryWriteAdd(t *testing.T) {
	s := newMemoryTestStore(t)
	w := newMemoryWrite(s, "proj")

	args, _ := json.Marshal(memoryWriteArgs{Content: "the user likes tabs, not spaces"})
	out, err := w.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "saved memory entry") {
		t.Errorf("expected a success message, got: %q", out)
	}

	entries, _ := s.RecentMemory([]string{"proj"}, 8)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry in the store, got %d", len(entries))
	}
}

func TestMemoryWriteOverflowReturnsActionableError(t *testing.T) {
	s := newMemoryTestStore(t)
	w := newMemoryWrite(s, "proj")

	// Fill the scope right up to the cap.
	filler := strings.Repeat("x", maxScopeMemoryChars-10)
	args, _ := json.Marshal(memoryWriteArgs{Content: filler})
	if _, err := w.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	// This write would push it over the cap.
	overflowArgs, _ := json.Marshal(memoryWriteArgs{Content: strings.Repeat("y", 50)})
	out, err := w.Execute(context.Background(), overflowArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "full") {
		t.Errorf("expected an overflow message mentioning the scope is full, got: %q", out)
	}
	if !strings.Contains(out, "#1") {
		t.Errorf("overflow message should list the existing entry's id so the model can consolidate, got: %q", out)
	}

	// The overflowing content must not have been persisted.
	entries, _ := s.RecentMemory([]string{"proj"}, 8)
	if len(entries) != 1 {
		t.Errorf("overflow write should not persist — got %d entries", len(entries))
	}
}

func TestMemoryWriteReplace(t *testing.T) {
	s := newMemoryTestStore(t)
	entry, err := s.WriteMemoryEntry("proj", "original", false)
	if err != nil {
		t.Fatal(err)
	}

	w := newMemoryWrite(s, "proj")
	args, _ := json.Marshal(memoryWriteArgs{Operation: "replace", ID: entry.ID, Content: "consolidated"})
	if _, err := w.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	entries, _ := s.RecentMemory([]string{"proj"}, 8)
	if len(entries) != 1 || entries[0].Content != "consolidated" {
		t.Errorf("expected the entry to be replaced in place, got %+v", entries)
	}
}

func TestMemoryWriteReplaceRecoversFromOverflow(t *testing.T) {
	// The whole point of replace/remove existing during overflow: they
	// must work even when the scope is completely full, since that's
	// exactly the situation they exist to resolve.
	s := newMemoryTestStore(t)
	entry, err := s.WriteMemoryEntry("proj", strings.Repeat("x", maxScopeMemoryChars), false)
	if err != nil {
		t.Fatal(err)
	}

	w := newMemoryWrite(s, "proj")
	args, _ := json.Marshal(memoryWriteArgs{Operation: "replace", ID: entry.ID, Content: "much shorter now"})
	out, err := w.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced") {
		t.Errorf("expected a replace success message, got: %q", out)
	}
}

func TestMemoryWriteRemove(t *testing.T) {
	s := newMemoryTestStore(t)
	entry, _ := s.WriteMemoryEntry("proj", "temporary", false)

	w := newMemoryWrite(s, "proj")
	args, _ := json.Marshal(memoryWriteArgs{Operation: "remove", ID: entry.ID})
	if _, err := w.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	entries, _ := s.RecentMemory([]string{"proj"}, 8)
	if len(entries) != 0 {
		t.Errorf("expected the entry to be gone, got %+v", entries)
	}
}

func TestMemoryWriteGlobalScope(t *testing.T) {
	s := newMemoryTestStore(t)
	w := newMemoryWrite(s, "/some/project/path")

	args, _ := json.Marshal(memoryWriteArgs{Content: "true everywhere", Scope: "global"})
	if _, err := w.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	projectScoped, _ := s.RecentMemory([]string{"/some/project/path"}, 8)
	if len(projectScoped) != 0 {
		t.Error("a global-scoped write should not show up under the project scope")
	}
	globalScoped, _ := s.RecentMemory([]string{store.GlobalScope}, 8)
	if len(globalScoped) != 1 {
		t.Error("a global-scoped write should show up under GlobalScope")
	}
}

func TestMemorySearch(t *testing.T) {
	s := newMemoryTestStore(t)
	_, _ = s.WriteMemoryEntry("proj", "the deploy script is scripts/deploy.sh", false)

	search := newMemorySearch(s, "proj")
	args, _ := json.Marshal(memorySearchArgs{Query: "deploy"})
	out, err := search.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deploy.sh") {
		t.Errorf("expected the deploy entry in results, got: %q", out)
	}
}
