package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func toolCall(id, name, args string) openai.ToolCall {
	return openai.ToolCall{ID: id, Function: openai.ToolCallFunction{Name: name, Arguments: args}}
}

func autoCheckpointCount(t *testing.T, s *Service) int {
	t.Helper()
	snaps, err := s.tools.Snapshots().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, sn := range snaps {
		if strings.HasPrefix(sn.Message, AutoCheckpointPrefix) {
			n++
		}
	}
	return n
}

// TestAutoCheckpointBeforeFirstMutationOnly (#113): a run that mutates
// takes exactly ONE automatic checkpoint, captured before the first
// mutating batch — so rewinding it undoes everything the turn changed,
// including later batches.
func TestAutoCheckpointBeforeFirstMutationOnly(t *testing.T) {
	workspace := t.TempDir()
	srv, _ := fakeGateway(t, []scriptedChatResponse{
		{toolCalls: []openai.ToolCall{toolCall("c1", "list_dir", `{"path":""}`)}}, // read-only batch first: no checkpoint yet
		{toolCalls: []openai.ToolCall{toolCall("c2", "write_file", `{"path":"out.txt","content":"v1"}`)}},
		{toolCalls: []openai.ToolCall{toolCall("c3", "write_file", `{"path":"out.txt","content":"v2"}`)}},
		{content: "done"},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "change stuff", nil, nil); err != nil {
		t.Fatal(err)
	}

	if got := autoCheckpointCount(t, s); got != 1 {
		t.Fatalf("auto checkpoints after a mutating run = %d, want exactly 1", got)
	}

	// The checkpoint predates the first write: rewinding must remove
	// out.txt entirely, not roll it back to v1.
	snap, ok, err := s.LatestAutoCheckpoint(context.Background())
	if err != nil || !ok {
		t.Fatalf("LatestAutoCheckpoint = ok=%v err=%v", ok, err)
	}
	if _, _, err := s.Rewind(context.Background(), snap.ID); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("rewind should remove the file created after the checkpoint, stat err = %v", err)
	}
}

// TestReadOnlyRunTakesNoCheckpoint: turns that never mutate never pay
// for a snapshot.
func TestReadOnlyRunTakesNoCheckpoint(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := fakeGateway(t, []scriptedChatResponse{
		{toolCalls: []openai.ToolCall{toolCall("c1", "read_file", `{"path":"a.txt"}`)}},
		{content: "done"},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "just read", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := autoCheckpointCount(t, s); got != 0 {
		t.Fatalf("read-only run took %d checkpoints, want 0", got)
	}
	if _, ok, _ := s.LatestAutoCheckpoint(context.Background()); ok {
		t.Fatal("LatestAutoCheckpoint should report none for a read-only history")
	}
}
