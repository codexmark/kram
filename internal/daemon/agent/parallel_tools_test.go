package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/daemon/tools"
	"github.com/codexmark/kram/internal/openai"
)

func TestIsReadOnlyAllowlist(t *testing.T) {
	for name, want := range map[string]bool{
		"read_file": true, "grep": true, "web_fetch": true, "snapshot_diff": true,
		"bash": false, "write_file": false, "delete_file": false,
		"ask_question": false, "delegate_task": false, "snapshot_restore": false,
		"lsp_diagnostics":        false, // deliberately excluded until the manager's concurrency is proven
		"mcp__github__get_issue": false, "made_up_tool": false,
	} {
		if got := tools.IsReadOnly(name); got != want {
			t.Errorf("IsReadOnly(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestReadOnlyBatchRunsConcurrently: three slow read-only calls
// (web_fetch against a deliberately slow server) must overlap — the
// batch finishes in roughly one delay, not three.
func TestReadOnlyBatchRunsConcurrently(t *testing.T) {
	const delay = 120 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		fmt.Fprint(w, "slow page")
	}))
	defer srv.Close()

	workspace := t.TempDir()
	s := &Service{tools: tools.NewRegistry(workspace, nil, nil), heartbeatInterval: time.Hour}

	fetch := func(id string) openai.ToolCall {
		return openai.ToolCall{ID: id, Function: openai.ToolCallFunction{
			Name: "web_fetch", Arguments: fmt.Sprintf(`{"url":%q}`, srv.URL),
		}}
	}
	start := time.Now()
	outcomes := s.runToolBatch(context.Background(), []openai.ToolCall{fetch("a"), fetch("b"), fetch("c")}, nil)
	elapsed := time.Since(start)

	for i, out := range outcomes {
		if !out.activity.OK || !strings.Contains(out.msg.Content, "slow page") {
			t.Fatalf("outcome %d = %+v", i, out.activity)
		}
	}
	if elapsed > 2*delay {
		t.Fatalf("three %v read-only calls took %v — they did not overlap", delay, elapsed)
	}
}

// TestMixedBatchPreservesOrderAndSequencesMutations: a write between two
// reads keeps its exact position, and every outcome lands at its call's
// index regardless of how the group executed.
func TestMixedBatchPreservesOrderAndSequencesMutations(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Service{tools: tools.NewRegistry(workspace, nil, nil), heartbeatInterval: time.Hour}

	calls := []openai.ToolCall{
		{ID: "1", Function: openai.ToolCallFunction{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
		{ID: "2", Function: openai.ToolCallFunction{Name: "write_file", Arguments: `{"path":"b.txt","content":"second"}`}},
		{ID: "3", Function: openai.ToolCallFunction{Name: "read_file", Arguments: `{"path":"b.txt"}`}},
	}
	outcomes := s.runToolBatch(context.Background(), calls, nil)

	if !strings.Contains(outcomes[0].msg.Content, "first") {
		t.Fatalf("outcome 0 = %q", outcomes[0].msg.Content)
	}
	if !outcomes[1].activity.OK {
		t.Fatalf("write failed: %+v", outcomes[1].activity)
	}
	// The read AFTER the write must see the write's effect — proof the
	// mutation kept its sequential position instead of racing the reads.
	if !strings.Contains(outcomes[2].msg.Content, "second") {
		t.Fatalf("read-after-write = %q, want the written content", outcomes[2].msg.Content)
	}
	for i, out := range outcomes {
		if out.msg.ToolCallID != calls[i].ID {
			t.Fatalf("outcome %d belongs to call %q — order lost", i, out.msg.ToolCallID)
		}
	}
}
