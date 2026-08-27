package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

// wireContains reports whether any system message of the i-th captured
// request contains substr.
func wireContains(reqs [][]openai.ChatMessage, i int, substr string) bool {
	if i >= len(reqs) {
		return false
	}
	for _, m := range reqs[i] {
		if m.Role == "system" && strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

// TestVerifyGateNudgesUnverifiedSourceChange (#116): a run that writes a
// source file and answers without any build/test gets exactly one
// structural nudge, then the verified answer is accepted.
func TestVerifyGateNudgesUnverifiedSourceChange(t *testing.T) {
	workspace := t.TempDir()
	srv, requests := fakeGateway(t, []scriptedChatResponse{
		{toolCalls: []openai.ToolCall{toolCall("c1", "write_file", `{"path":"main.go","content":"package main"}`)}},
		{content: "changed it, all done"}, // final answer, unverified → gate trips
		{toolCalls: []openai.ToolCall{toolCall("c2", "bash", `{"command":"true"}`)}},
		{content: "verified: build passes"},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	res, err := s.Run(context.Background(), "sess-1", "edit main.go", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "verified: build passes" {
		t.Fatalf("final message = %q, want the post-verification answer", res.Message.Content)
	}
	reqs := requests()
	if len(reqs) != 4 {
		t.Fatalf("model calls = %d, want 4 (write, gated answer, verify, final)", len(reqs))
	}
	// The nudge appears exactly on the call after the gated answer — not
	// before it, and not on the final call after the model reacted.
	if wireContains(reqs, 1, "verification gate") {
		t.Fatal("nudge must not appear before the gate trips")
	}
	if !wireContains(reqs, 2, "verification gate") {
		t.Fatal("call after the gated answer must carry the verify nudge")
	}
	if wireContains(reqs, 3, "verification gate") {
		t.Fatal("nudge must be one-shot, not repeated on later calls")
	}
	// The pre-verification answer is preserved in history, not discarded.
	all, err := s.store.ListMessages("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range all {
		if m.Role == "assistant" && m.Content == "changed it, all done" {
			found = true
		}
	}
	if !found {
		t.Fatal("the gated (pre-verification) answer should be persisted, not thrown away")
	}
}

// TestVerifyGateAcceptsVerifiedRun: a bash after the mutation satisfies
// the gate — the final answer is accepted with no extra round.
func TestVerifyGateAcceptsVerifiedRun(t *testing.T) {
	workspace := t.TempDir()
	srv, requests := fakeGateway(t, []scriptedChatResponse{
		{toolCalls: []openai.ToolCall{toolCall("c1", "write_file", `{"path":"main.go","content":"package main"}`)}},
		{toolCalls: []openai.ToolCall{toolCall("c2", "bash", `{"command":"true"}`)}},
		{content: "done, tests ran"},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	res, err := s.Run(context.Background(), "sess-1", "edit and test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "done, tests ran" {
		t.Fatalf("final message = %q", res.Message.Content)
	}
	if got := len(requests()); got != 3 {
		t.Fatalf("model calls = %d, want 3 (no gate round for a verified run)", got)
	}
}

// TestVerifyGateSkipsDocOnly: prose-only edits never trip the gate.
func TestVerifyGateSkipsDocOnly(t *testing.T) {
	workspace := t.TempDir()
	srv, requests := fakeGateway(t, []scriptedChatResponse{
		{toolCalls: []openai.ToolCall{toolCall("c1", "write_file", `{"path":"README.md","content":"# hi"}`)}},
		{content: "docs updated"},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	res, err := s.Run(context.Background(), "sess-1", "fix readme", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "docs updated" {
		t.Fatalf("final message = %q", res.Message.Content)
	}
	if got := len(requests()); got != 2 {
		t.Fatalf("model calls = %d, want 2 (doc-only run must not be gated)", got)
	}
}

// TestVerifyGateNeverLoops: a model that ignores the nudge and answers
// again without verifying is accepted on the second answer — the gate can
// add at most one round, ever.
func TestVerifyGateNeverLoops(t *testing.T) {
	workspace := t.TempDir()
	srv, requests := fakeGateway(t, []scriptedChatResponse{
		{toolCalls: []openai.ToolCall{toolCall("c1", "write_file", `{"path":"main.go","content":"package main"}`)}},
		{content: "done (no verification)"},
		{content: "still done, verification is not applicable here"},
	})
	defer srv.Close()
	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	res, err := s.Run(context.Background(), "sess-1", "edit main.go", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "still done, verification is not applicable here" {
		t.Fatalf("final message = %q", res.Message.Content)
	}
	if got := len(requests()); got != 3 {
		t.Fatalf("model calls = %d, want exactly 3 (one gate round, never two)", got)
	}
}

// TestVerifyTrackerBatchOrder: within one batch, order decides — a bash
// before the mutation does not verify it; a bash after it does.
func TestVerifyTrackerBatchOrder(t *testing.T) {
	var v verifyTracker
	v.observe([]openai.ToolCall{
		toolCall("a", "bash", `{"command":"go test"}`),
		toolCall("b", "edit_file", `{"path":"x.go","old_string":"a","new_string":"b"}`),
	})
	if !v.pending {
		t.Fatal("bash before the mutation must not count as verification")
	}
	v = verifyTracker{}
	v.observe([]openai.ToolCall{
		toolCall("a", "edit_file", `{"path":"x.go","old_string":"a","new_string":"b"}`),
		toolCall("b", "bash", `{"command":"go test"}`),
	})
	if v.pending {
		t.Fatal("bash after the mutation must count as verification")
	}
}

func TestMutationIsDocOnly(t *testing.T) {
	cases := []struct {
		name, tool, args string
		want             bool
	}{
		{"markdown write", "write_file", `{"path":"docs/guide.md"}`, true},
		{"txt delete", "delete_file", `{"path":"NOTES.txt"}`, true},
		{"go edit", "edit_file", `{"path":"main.go"}`, false},
		{"no extension", "write_file", `{"path":"Makefile"}`, false},
		{"empty path", "write_file", `{}`, false},
		{"unparseable", "write_file", `not json`, false},
		{"move doc to doc", "move_file", `{"old_path":"a.md","new_path":"b.md"}`, true},
		{"move doc to source", "move_file", `{"old_path":"a.md","new_path":"b.go"}`, false},
		{"case-insensitive ext", "write_file", `{"path":"README.MD"}`, true},
	}
	for _, tc := range cases {
		if got := mutationIsDocOnly(tc.tool, tc.args); got != tc.want {
			t.Errorf("%s: mutationIsDocOnly(%s, %s) = %v, want %v", tc.name, tc.tool, tc.args, got, tc.want)
		}
	}
}
