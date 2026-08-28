package agent

import (
	"strings"
	"testing"
)

// baseSectionOrder is the same fixed order compileBaseSections
// (promptcompiler.go) turns into PromptParts — duplicated here as the
// test's own independent expectation of what "the documented order"
// means, rather than deriving it from the function under test.
var baseSectionOrder = []string{
	"identity", "workflow", "skills", "memory-policy",
	"delegation", "asking", "tasks", "coding-policy", "output", "examples", "safety",
}

// TestCompileBaseSectionsMatchesSystemPromptByteForByte is the golden-file
// regression test the Model/Agent Profile phase's own issue called for:
// splitting systemPrompt()'s one template into nine named sections must
// not change a single byte of what the model actually receives in the
// baseline (no SystemPromptOverride) case. Joining the split sections'
// Content with the same "\n" separator systemPrompt() itself uses must
// reproduce its output exactly.
func TestCompileBaseSectionsMatchesSystemPromptByteForByte(t *testing.T) {
	workspace := "/some/workspace"
	parts := compileBaseSections(workspace, ProfileCompact)

	contents := make([]string, len(parts))
	for i, p := range parts {
		contents[i] = p.Content
	}
	got := strings.Join(contents, "\n")
	want := systemPrompt(workspace)
	if got != want {
		t.Errorf("compileBaseSections joined != systemPrompt():\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestCompileBaseSectionsOrderAndOwnership pins the documented order and
// that every part is uniquely and correctly named/tagged — an explicit,
// checkable property instead of something only visible by reading prose
// closely, per the issue's own stated motivation.
func TestCompileBaseSectionsOrderAndOwnership(t *testing.T) {
	parts := compileBaseSections("/ws", ProfileCompact)

	if len(parts) != len(baseSectionOrder) {
		t.Fatalf("parts = %d, want %d: %+v", len(parts), len(baseSectionOrder), parts)
	}
	seen := make(map[string]bool)
	for i, p := range parts {
		if p.ID != baseSectionOrder[i] {
			t.Errorf("order[%d] = %q, want %q", i, p.ID, baseSectionOrder[i])
		}
		if seen[p.ID] {
			t.Errorf("duplicate section ID %q", p.ID)
		}
		seen[p.ID] = true
		if p.Placement != PlacementPreamble || p.Refresh != RefreshStatic || p.Source != "builtin" {
			t.Errorf("section %q = %+v, want Placement=Preamble Refresh=Static Source=builtin", p.ID, p)
		}
		if strings.TrimSpace(p.Content) == "" {
			t.Errorf("section %q has empty content", p.ID)
		}
	}
}

// TestCompileBaseSectionsIdentityCarriesWorkspaceAndOS confirms the one
// section with real inputs (identitySection) actually threads them
// through, rather than every section silently being a hardcoded const.
func TestCompileBaseSectionsIdentityCarriesWorkspaceAndOS(t *testing.T) {
	parts := compileBaseSections("/my/project/root", ProfileCompact)
	identity := parts[0]
	if identity.ID != "identity" {
		t.Fatalf("parts[0].ID = %q, want identity", identity.ID)
	}
	if !strings.Contains(identity.Content, "/my/project/root") {
		t.Errorf("identity section missing the workspace path: %q", identity.Content)
	}
	if !strings.Contains(identity.Content, "Environment:") {
		t.Errorf("identity section missing the OS description framing: %q", identity.Content)
	}
}
