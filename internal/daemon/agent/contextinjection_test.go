package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/daemon/compaction"
	"github.com/codexmark/kram/internal/daemon/store"
)

// TestNeedsFreshInjection covers the pure decision function directly:
// nothing persisted yet, an exact match still in effective history, and a
// content mismatch (changed AGENTS.md/memory).
func TestNeedsFreshInjection(t *testing.T) {
	empty := []store.Message{}
	if !needsFreshInjection(empty, projectContextMarkerName, "fresh") {
		t.Error("no prior marker at all should require injection")
	}

	matching := []store.Message{{Role: "system", Name: projectContextMarkerName, Content: "fresh"}}
	if needsFreshInjection(matching, projectContextMarkerName, "fresh") {
		t.Error("an exact content match still in effective history should skip injection")
	}

	stale := []store.Message{{Role: "system", Name: projectContextMarkerName, Content: "old"}}
	if !needsFreshInjection(stale, projectContextMarkerName, "new") {
		t.Error("changed content should require a fresh injection")
	}
}

// TestNeedsFreshInjectionUsesTheLastMatchingMarker confirms multiple
// historical markers don't confuse the scan — only the most recent one
// (mirroring compaction.EffectiveHistory's own "last one wins" behavior)
// decides the outcome.
func TestNeedsFreshInjectionUsesTheLastMatchingMarker(t *testing.T) {
	history := []store.Message{
		{Role: "system", Name: projectContextMarkerName, Content: "v1"},
		{Role: "user", Content: "hi"},
		{Role: "system", Name: projectContextMarkerName, Content: "v2"},
	}
	if needsFreshInjection(history, projectContextMarkerName, "v2") {
		t.Error("fresh content matching the LAST marker (v2) should skip injection")
	}
	if !needsFreshInjection(history, projectContextMarkerName, "v1") {
		t.Error("fresh content matching only an earlier, superseded marker (v1) should still require injection — the scan must use the last one, not any match")
	}
}

// TestRunLoopSkipsUnchangedProjectContextAndMemoryOnSecondRun is the
// actual token-savings contract issue #27 exists for: unchanged content
// across turns must not be resent as a duplicate fresh preamble part once
// it's already persisted in this session's own history from an earlier
// turn.
func TestRunLoopSkipsUnchangedProjectContextAndMemoryOnSecondRun(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("Always run tests."), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, requests := fakeGateway(t, []scriptedChatResponse{{content: "ok"}, {content: "ok again"}})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	if _, err := s.store.WriteMemoryEntry(workspace, "the user prefers terse answers", false); err != nil {
		t.Fatal(err)
	}
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "first", nil, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := s.Run(context.Background(), "sess-1", "second", nil, nil); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 gateway requests, got %d", len(reqs))
	}

	wantProjectContext := formatProjectContextContent("Always run tests.")
	wantMemorySubstring := "the user prefers terse answers"

	firstHasProjectContext, firstHasMemory := false, false
	for _, m := range reqs[0] {
		if m.Content == wantProjectContext {
			firstHasProjectContext = true
		}
		if strings.Contains(m.Content, wantMemorySubstring) {
			firstHasMemory = true
		}
	}
	if !firstHasProjectContext || !firstHasMemory {
		t.Fatalf("first run should inject both project-context and memory fresh, got: %+v", reqs[0])
	}

	secondProjectContextCount, secondMemoryCount := 0, 0
	for _, m := range reqs[1] {
		if m.Content == wantProjectContext {
			secondProjectContextCount++
		}
		if strings.Contains(m.Content, wantMemorySubstring) {
			secondMemoryCount++
		}
	}
	if secondProjectContextCount != 1 {
		t.Errorf("second run project-context occurrences = %d, want exactly 1 (from persisted history, not a fresh duplicate resend)", secondProjectContextCount)
	}
	if secondMemoryCount != 1 {
		t.Errorf("second run memory occurrences = %d, want exactly 1 (from persisted history, not a fresh duplicate resend)", secondMemoryCount)
	}
}

// TestRunLoopReinjectsProjectContextWhenAgentsMdChanges confirms a real
// content change between two runs still gets a fresh injection — the
// change-detection gate must not become a permanent "never resend again"
// switch.
func TestRunLoopReinjectsProjectContextWhenAgentsMdChanges(t *testing.T) {
	workspace := t.TempDir()
	agentsPath := filepath.Join(workspace, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("v1 rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, requests := fakeGateway(t, []scriptedChatResponse{{content: "ok"}, {content: "ok again"}})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "first", nil, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := os.WriteFile(agentsPath, []byte("v2 rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), "sess-1", "second", nil, nil); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 gateway requests, got %d", len(reqs))
	}
	foundV2 := false
	for _, m := range reqs[1] {
		if m.Content == formatProjectContextContent("v2 rules") {
			foundV2 = true
		}
	}
	if !foundV2 {
		t.Fatalf("second run should have injected the changed (v2) AGENTS.md content fresh, got: %+v", reqs[1])
	}
}

// TestContextUsageOmitsProjectContextCategoryOnceAlreadyPersisted confirms
// ContextUsage mirrors runLoop's own change-detection gate rather than
// unconditionally reporting a fresh "project_context" category every
// time — otherwise, once a marker is persisted in history (counted as
// ordinary message tokens), a naive unconditional compilePreamble call
// here would double-count the same content under two categories.
func TestContextUsageOmitsProjectContextCategoryOnceAlreadyPersisted(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("stable rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := fakeGateway(t, []scriptedChatResponse{{content: "ok"}})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	before, err := s.ContextUsage(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	foundBefore := false
	for _, cat := range before.Categories {
		if cat.Name == "project_context" {
			foundBefore = true
		}
	}
	if !foundBefore {
		t.Fatalf("before any turn, project_context should be reported as its own category, got: %+v", before.Categories)
	}

	if _, err := s.Run(context.Background(), "sess-1", "first", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	after, err := s.ContextUsage(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, cat := range after.Categories {
		if cat.Name == "project_context" {
			t.Fatalf("after the marker is already persisted in history, project_context should not be reported as a separate fresh category (would double-count), got: %+v", after.Categories)
		}
	}
	if after.Used < before.Used {
		t.Errorf("used tokens after persisting history should not have gone down: before=%d after=%d", before.Used, after.Used)
	}
}

// TestRunLoopReinjectsAfterCompactionPrunesThePriorMarker is the
// regression test for the issue's own "trickiest part": a compaction
// pass that prunes the message carrying the last injection must force a
// fresh reinjection on the next eligible turn, even though the raw
// content itself hasn't changed. Compaction is simulated directly by
// appending a real compaction marker (rather than fighting token-budget
// thresholds to trigger one for real) — this isolates exactly the
// mechanism under test: compaction.EffectiveHistory's own truncation,
// which needsFreshInjection relies on rather than reimplementing.
func TestRunLoopReinjectsAfterCompactionPrunesThePriorMarker(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("stable rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, requests := fakeGateway(t, []scriptedChatResponse{{content: "ok"}, {content: "ok again"}})
	defer srv.Close()

	s := newTestService(t, workspace, srv.URL, Config{Workspace: workspace, MaxTurns: 10})
	newTestSession(t, s, "sess-1")

	if _, err := s.Run(context.Background(), "sess-1", "first", nil, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Simulate compaction summarizing everything up to and including the
	// first run's project-context marker away.
	if _, err := s.store.AppendMessage("sess-1", store.Message{
		Role: "system", Name: compaction.CompactionMarkerName, Content: "PRIOR SESSION CONTEXT — reference only.",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Run(context.Background(), "sess-1", "second", nil, nil); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 gateway requests, got %d", len(reqs))
	}
	foundFreshInjection := false
	for _, m := range reqs[1] {
		if m.Content == formatProjectContextContent("stable rules") {
			foundFreshInjection = true
		}
	}
	if !foundFreshInjection {
		t.Fatalf("after compaction pruned the prior marker, the next turn should reinject even though content is unchanged, got: %+v", reqs[1])
	}
}
