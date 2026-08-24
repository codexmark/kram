package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/daemon/tools"
	"github.com/codexmark/kram/internal/openai"
)

func TestCompilePreambleBaseOnly(t *testing.T) {
	parts := compilePreamble("/ws", "", false, openai.ChatMessage{}, false, nil, nil)

	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1 (base only): %+v", len(parts), parts)
	}
	p := parts[0]
	if p.ID != "base" || p.Placement != PlacementPreamble || p.Refresh != RefreshStatic || p.Source != "builtin" {
		t.Errorf("base part = %+v, want ID=base Placement=Preamble Refresh=Static Source=builtin", p)
	}
	if !strings.Contains(p.Content, "Kram") {
		t.Errorf("base part content doesn't look like systemPrompt output: %q", p.Content[:min(80, len(p.Content))])
	}
}

func TestCompilePreambleWithProjectContextOnly(t *testing.T) {
	parts := compilePreamble("/ws", "some AGENTS.md text", true, openai.ChatMessage{}, false, nil, nil)

	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (base + project-context): %+v", len(parts), parts)
	}
	p := parts[1]
	if p.ID != "project-context" || p.Placement != PlacementPreamble || p.Refresh != RefreshIteration || p.Source != "AGENTS.md" {
		t.Errorf("project-context part = %+v, want ID=project-context Placement=Preamble Refresh=Iteration Source=AGENTS.md", p)
	}
	if !strings.Contains(p.Content, "some AGENTS.md text") {
		t.Errorf("project-context content missing the actual text: %q", p.Content)
	}
	if !strings.Contains(p.Content, "AGENTS.md/CLAUDE.md") {
		t.Errorf("project-context content missing the framing header: %q", p.Content)
	}
}

func TestCompilePreambleWithMemoryOnly(t *testing.T) {
	memMsg := openai.ChatMessage{Role: "system", Content: "remembered fact"}
	parts := compilePreamble("/ws", "", false, memMsg, true, nil, nil)

	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 (base + memory): %+v", len(parts), parts)
	}
	p := parts[1]
	if p.ID != "memory" || p.Placement != PlacementPreamble || p.Refresh != RefreshRun || p.Source != "memory" {
		t.Errorf("memory part = %+v, want ID=memory Placement=Preamble Refresh=Run Source=memory", p)
	}
	if p.Content != "remembered fact" {
		t.Errorf("memory part content = %q, want the memoryMsg's own Content passed through unchanged", p.Content)
	}
}

func TestCompilePreambleWithBothProjectContextAndMemory(t *testing.T) {
	memMsg := openai.ChatMessage{Content: "remembered fact"}
	parts := compilePreamble("/ws", "ctx", true, memMsg, true, nil, nil)

	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3 (base, project-context, memory): %+v", len(parts), parts)
	}
	gotIDs := []string{parts[0].ID, parts[1].ID, parts[2].ID}
	wantIDs := []string{"base", "project-context", "memory"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("order[%d] = %q, want %q (full order: %v)", i, gotIDs[i], wantIDs[i], gotIDs)
		}
	}
}

// TestCompileToolsOverviewNilRegistryReturnsEmptyPart matches
// compilePreamble's own "only include if present" handling — nil reg is
// the shape evals/tests without a tool registry produce.
func TestCompileToolsOverviewNilRegistryReturnsEmptyPart(t *testing.T) {
	p := compileToolsOverview(nil, nil)
	if p.ID != "tools-overview" || p.Content != "" {
		t.Errorf("nil registry part = %+v, want ID=tools-overview and empty Content", p)
	}
}

// TestCompileToolsOverviewListsEnabledToolsAndSkipsDisabled uses a real
// tools.Registry (NewRegistry takes disabled names directly, so no fakes
// needed) — confirms an enabled tool with hand-curated ToolMetadata
// renders its PreferOver cross-reference, a disabled tool is omitted
// entirely, and a tool with no curated metadata still appears via the
// Description()-derived fallback.
func TestCompileToolsOverviewListsEnabledToolsAndSkipsDisabled(t *testing.T) {
	reg := tools.NewRegistry(t.TempDir(), nil, map[string]bool{"bash": true})

	p := compileToolsOverview(reg, nil)

	if !strings.HasPrefix(p.Content, "# Tools\n") {
		t.Errorf("content should start with the # Tools header, got %q", p.Content[:min(40, len(p.Content))])
	}
	if !strings.Contains(p.Content, "run_background") || !strings.Contains(p.Content, "(use this instead of bash)") {
		t.Errorf("run_background should appear with its PreferOver cross-reference, got: %s", p.Content)
	}
	if strings.Contains(p.Content, "bash — ") {
		t.Errorf("disabled tool bash should not appear in the overview, got: %s", p.Content)
	}
	if !strings.Contains(p.Content, "web_fetch — ") {
		t.Errorf("web_fetch (no curated metadata) should still appear via the Description() fallback, got: %s", p.Content)
	}
	if !strings.Contains(p.Content, "Call independent tools in the same turn") {
		t.Errorf("content should end with the batching footer, got: %s", p.Content)
	}
}

// TestCompileToolsOverviewExcludesPermissionFullyDeniedTool is the
// regression test for a real bug a review found: this function used to
// build its list from reg.AllTools(), which only excludes settings-
// disabled tools — a tool the permission policy denies unconditionally
// (exactly what a Strict preset's "delete_file: deny *" rule produces)
// stayed AllTools()-visible and so got announced in the prompt with no
// matching function in Definitions()'s wire schema, the model's actual
// tool-calling surface. reg.VisibleTools() (internal/daemon/tools) is
// now the one source both Definitions() and this function read from, so
// this drives a real permissions.json on disk — the same mechanism a
// Strict preset uses — rather than a settings-disabled map, which is
// the one difference from TestCompileToolsOverviewListsEnabledToolsAnd
// SkipsDisabled above and the reason that test alone didn't catch this.
func TestCompileToolsOverviewExcludesPermissionFullyDeniedTool(t *testing.T) {
	workspace := t.TempDir()
	kramDir := filepath.Join(workspace, ".kram")
	if err := os.MkdirAll(kramDir, 0o755); err != nil {
		t.Fatal(err)
	}
	permJSON := `{"rules":[{"tool":"delete_file","decision":"deny"}]}`
	if err := os.WriteFile(filepath.Join(kramDir, "permissions.json"), []byte(permJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := tools.NewRegistry(workspace, nil, nil)
	p := compileToolsOverview(reg, nil)

	if strings.Contains(p.Content, "delete_file — ") {
		t.Errorf("a permission-denied tool must not be announced in the prompt with no matching wire-schema function, got: %s", p.Content)
	}
	foundInAllTools := false
	for _, info := range reg.AllTools() {
		if info.Name == "delete_file" {
			foundInAllTools = true
			if info.Disabled {
				t.Error("delete_file should read Disabled=false in AllTools() — permission-denied is a different axis from settings-disabled")
			}
		}
	}
	if !foundInAllTools {
		t.Fatal("test setup issue: delete_file should still be a registered tool")
	}
}

// TestCompileToolsOverviewHonorsToolOrder confirms a configured order
// changes the overview's rendered line order, not just VisibleTools()'s
// own default alphabetical sort — the presentation-only ordering feature
// this function exists to support (see tools.OrderToolNames).
func TestCompileToolsOverviewHonorsToolOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate from any real global permissions.json
	reg := tools.NewRegistry(t.TempDir(), nil, nil)

	p := compileToolsOverview(reg, []string{"run_background", tools.ToolOrderRest})

	runBackgroundLine := strings.Index(p.Content, "run_background — ")
	bashLine := strings.Index(p.Content, "bash — ")
	if runBackgroundLine == -1 || bashLine == -1 {
		t.Fatalf("expected both run_background and bash to appear, got: %s", p.Content)
	}
	if runBackgroundLine > bashLine {
		t.Errorf("run_background should be listed before bash when explicitly ordered first, got:\n%s", p.Content)
	}
}

func TestCompileBackgroundJobGuidanceNilRegistryReturnsEmptyPart(t *testing.T) {
	p := compileBackgroundJobGuidance(nil)
	if p.ID != "background-job-guidance" || p.Content != "" {
		t.Errorf("nil registry part = %+v, want ID=background-job-guidance and empty Content", p)
	}
}

// TestCompileBackgroundJobGuidancePresentWhenRunBackgroundVisible confirms
// the guidance appears — and doesn't overclaim a notification capability
// Kram's daemon doesn't have — when run_background is actually offered.
func TestCompileBackgroundJobGuidancePresentWhenRunBackgroundVisible(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := tools.NewRegistry(t.TempDir(), nil, nil)

	p := compileBackgroundJobGuidance(reg)

	if p.Content == "" {
		t.Fatal("expected non-empty guidance when run_background is visible")
	}
	if !strings.Contains(p.Content, "run_background") {
		t.Errorf("guidance should mention run_background, got: %s", p.Content)
	}
	if strings.Contains(strings.ToLower(p.Content), "notif") && !strings.Contains(p.Content, "no notification") {
		t.Errorf("guidance must not claim a notification capability Kram's daemon doesn't have, got: %s", p.Content)
	}
}

// TestCompileBackgroundJobGuidanceAbsentWhenRunBackgroundDisabled mirrors
// the same "don't tell the model about a workflow it can't use" discipline
// compileToolsOverview already applies via VisibleTools().
func TestCompileBackgroundJobGuidanceAbsentWhenRunBackgroundDisabled(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg := tools.NewRegistry(t.TempDir(), nil, map[string]bool{"run_background": true})

	p := compileBackgroundJobGuidance(reg)

	if p.Content != "" {
		t.Errorf("expected no guidance when run_background is settings-disabled, got: %s", p.Content)
	}
}

// TestCompileBackgroundJobGuidanceAbsentWhenPermissionDenied covers the
// other axis VisibleTools() checks — a Strict-style deny-all policy, not
// just the settings toggle.
func TestCompileBackgroundJobGuidanceAbsentWhenPermissionDenied(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	kramDir := filepath.Join(workspace, ".kram")
	if err := os.MkdirAll(kramDir, 0o755); err != nil {
		t.Fatal(err)
	}
	permJSON := `{"rules":[{"tool":"run_background","decision":"deny"}]}`
	if err := os.WriteFile(filepath.Join(kramDir, "permissions.json"), []byte(permJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(workspace, nil, nil)

	p := compileBackgroundJobGuidance(reg)

	if p.Content != "" {
		t.Errorf("expected no guidance when run_background is permission-denied, got: %s", p.Content)
	}
}

func TestCompileTurnPostscriptNeitherFlag(t *testing.T) {
	parts := compileTurnPostscript(false, false)
	if len(parts) != 0 {
		t.Errorf("expected 0 parts when neither flag is set, got %+v", parts)
	}
}

func TestCompileTurnPostscriptEmptyRetryOnly(t *testing.T) {
	parts := compileTurnPostscript(true, false)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.ID != "empty-retry-nudge" || p.Placement != PlacementPostHistory || p.Refresh != RefreshIteration || p.Source != "runtime" {
		t.Errorf("part = %+v, want ID=empty-retry-nudge Placement=PostHistory Refresh=Iteration Source=runtime", p)
	}
	if !strings.Contains(p.Content, "empty") {
		t.Errorf("empty-retry content doesn't mention the empty response: %q", p.Content)
	}
}

func TestCompileTurnPostscriptNearBudgetOnly(t *testing.T) {
	parts := compileTurnPostscript(false, true)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.ID != "turn-budget-soft-landing" || p.Placement != PlacementPostHistory || p.Refresh != RefreshIteration || p.Source != "runtime" {
		t.Errorf("part = %+v, want ID=turn-budget-soft-landing Placement=PostHistory Refresh=Iteration Source=runtime", p)
	}
}

// TestCompileTurnPostscriptBothFlagsPreservesOrder pins the exact order
// the original inline code produced: the empty-retry nudge was appended
// immediately after history, the near-budget message later (after
// toolDefs was computed) — both land after history, empty-retry first.
func TestCompileTurnPostscriptBothFlagsPreservesOrder(t *testing.T) {
	parts := compileTurnPostscript(true, true)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2: %+v", len(parts), parts)
	}
	if parts[0].ID != "empty-retry-nudge" || parts[1].ID != "turn-budget-soft-landing" {
		t.Errorf("order = [%s, %s], want [empty-retry-nudge, turn-budget-soft-landing]", parts[0].ID, parts[1].ID)
	}
}

func TestPartsToMessagesRendersSystemRoleInOrder(t *testing.T) {
	parts := []PromptPart{
		{ID: "a", Content: "first"},
		{ID: "b", Content: "second"},
	}
	msgs := partsToMessages(parts)

	if len(msgs) != 2 {
		t.Fatalf("msgs = %d, want 2", len(msgs))
	}
	for i, want := range []string{"first", "second"} {
		if msgs[i].Role != "system" {
			t.Errorf("msgs[%d].Role = %q, want system", i, msgs[i].Role)
		}
		if msgs[i].Content != want {
			t.Errorf("msgs[%d].Content = %q, want %q", i, msgs[i].Content, want)
		}
	}
}

func TestPartsToMessagesEmptyInputProducesEmptyOutput(t *testing.T) {
	msgs := partsToMessages(nil)
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages for nil input, got %d", len(msgs))
	}
}
