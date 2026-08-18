package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/daemon/store"
)

func TestSessionSearchFindsRealMessage(t *testing.T) {
	s := newMemoryTestStore(t)
	if _, err := s.CreateSession("s1", "a real conversation"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage("s1", store.Message{Role: "user", Content: "how do I configure the widget-alpha subsystem"}); err != nil {
		t.Fatal(err)
	}

	tool := newSessionSearch(s)
	args, _ := json.Marshal(sessionSearchArgs{Query: "widget-alpha"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "widget-alpha") {
		t.Errorf("expected the matched content in the output, got: %s", out)
	}
	if !strings.Contains(out, "s1") {
		t.Errorf("expected the session id in the output, got: %s", out)
	}
}

func TestSessionSearchNoMatches(t *testing.T) {
	s := newMemoryTestStore(t)
	if _, err := s.CreateSession("s1", "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage("s1", store.Message{Role: "user", Content: "hello there"}); err != nil {
		t.Fatal(err)
	}

	tool := newSessionSearch(s)
	args, _ := json.Marshal(sessionSearchArgs{Query: "somethingnotpresentxyz"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if out != "no matches found" {
		t.Errorf("expected a clear no-matches message, got: %q", out)
	}
}

func TestSessionSearchEmptyQueryIsAnError(t *testing.T) {
	s := newMemoryTestStore(t)
	tool := newSessionSearch(s)
	args, _ := json.Marshal(sessionSearchArgs{Query: "   "})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "error:") {
		t.Errorf("expected an error string for an empty query, got: %q", out)
	}
}

func TestSessionSearchExcludesSubagentByDefaultAndIncludesWithScopeAll(t *testing.T) {
	s := newMemoryTestStore(t)
	if _, err := s.CreateSession("real", "chat with the user"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage("real", store.Message{Role: "user", Content: "let's talk about beacon-term"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession("sub", "subagent: handle beacon-term"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage("sub", store.Message{Role: "user", Content: "goal: beacon-term task"}); err != nil {
		t.Fatal(err)
	}

	tool := newSessionSearch(s)

	defaultArgs, _ := json.Marshal(sessionSearchArgs{Query: "beacon-term"})
	defaultOut, err := tool.Execute(context.Background(), defaultArgs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(defaultOut, "\"sub\"") || strings.Contains(defaultOut, "session sub ") {
		t.Errorf("expected the subagent session excluded by default, got: %s", defaultOut)
	}
	if !strings.Contains(defaultOut, "real") {
		t.Errorf("expected the real session's match, got: %s", defaultOut)
	}

	allArgs, _ := json.Marshal(sessionSearchArgs{Query: "beacon-term", Scope: "all"})
	allOut, err := tool.Execute(context.Background(), allArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(allOut, "subagent-run session") {
		t.Errorf("expected the subagent session to be included and flagged with scope=all, got: %s", allOut)
	}
}

func TestSessionSearchNeverShowsCompactionAsTheMatch(t *testing.T) {
	s := newMemoryTestStore(t)
	if _, err := s.CreateSession("s1", "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage("s1", store.Message{Role: "user", Content: "real talk about zephyr-term"}); err != nil {
		t.Fatal(err)
	}
	// A compaction-marker system message mentioning the same term must
	// never surface as if it were the actual matched message.
	if _, err := s.AppendMessage("s1", store.Message{
		Role:    "system",
		Name:    "kram:compaction_summary",
		Content: "PRIOR SESSION CONTEXT — reference only. zephyr-term zephyr-term zephyr-term.",
	}); err != nil {
		t.Fatal(err)
	}

	tool := newSessionSearch(s)
	args, _ := json.Marshal(sessionSearchArgs{Query: "zephyr-term"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "> [user]") {
		t.Errorf("expected the matched (>) line to be the real user message, got: %s", out)
	}
}
