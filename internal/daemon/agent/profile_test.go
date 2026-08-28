package agent

import (
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func TestProfileForModels(t *testing.T) {
	cases := []struct {
		name   string
		models []string
		want   PromptProfile
	}{
		{"all frontier", []string{"gpt-5.2", "claude-opus-5", "gemini-3-pro"}, ProfileFrontier},
		{"mixed pool downgrades", []string{"claude-opus-5", "qwen2.5-coder-14b"}, ProfileCompact},
		{"all small", []string{"qwen2.5-coder-14b", "llama-3.1-8b"}, ProfileCompact},
		{"empty list", nil, ProfileCompact},
		{"unpinned model", []string{"claude-opus-5", ""}, ProfileCompact},
		{"case-insensitive", []string{"Claude-Sonnet-4-5", "GPT-5"}, ProfileFrontier},
		{"unknown frontier-ish name stays compact", []string{"super-mega-100b"}, ProfileCompact},
		{"deepseek frontier", []string{"deepseek-v3.1"}, ProfileFrontier},
	}
	for _, tc := range cases {
		if got := ProfileForModels(tc.models); got != tc.want {
			t.Errorf("%s: ProfileForModels(%v) = %q, want %q", tc.name, tc.models, got, tc.want)
		}
	}
}

// TestCompileBaseSectionsCompactUnchanged: the zero-value profile must
// reproduce today's section list exactly — same IDs, same contents. The
// byte-for-byte parity with systemPrompt is separately pinned by
// systemprompt_test.go; this guards the ID sequence.
func TestCompileBaseSectionsCompactUnchanged(t *testing.T) {
	parts := compileBaseSections("/ws", ProfileCompact)
	wantIDs := []string{"identity", "workflow", "skills", "memory-policy", "delegation", "asking", "tasks", "coding-policy", "output", "examples", "safety"}
	if len(parts) != len(wantIDs) {
		t.Fatalf("compact sections = %d, want %d", len(parts), len(wantIDs))
	}
	for i, id := range wantIDs {
		if parts[i].ID != id {
			t.Fatalf("compact section[%d] = %s, want %s", i, parts[i].ID, id)
		}
	}
	if parts[1].Content != workflowSection || parts[8].Content != outputSection {
		t.Fatal("compact profile must use the original workflow/output sections")
	}
}

// TestCompileBaseSectionsFrontier (#130): frontier swaps workflow/output
// for their variants and omits the examples section entirely; everything
// else is untouched.
func TestCompileBaseSectionsFrontier(t *testing.T) {
	parts := compileBaseSections("/ws", ProfileFrontier)
	wantIDs := []string{"identity", "workflow", "skills", "memory-policy", "delegation", "asking", "tasks", "coding-policy", "output", "safety"}
	if len(parts) != len(wantIDs) {
		t.Fatalf("frontier sections = %d, want %d (no examples)", len(parts), len(wantIDs))
	}
	for i, id := range wantIDs {
		if parts[i].ID != id {
			t.Fatalf("frontier section[%d] = %s, want %s", i, parts[i].ID, id)
		}
	}
	if parts[1].Content != workflowSectionFrontier {
		t.Fatal("frontier profile must use workflowSectionFrontier")
	}
	if parts[8].Content != outputSectionFrontier {
		t.Fatal("frontier profile must use outputSectionFrontier")
	}
	if !strings.Contains(parts[1].Content, "one-sentence orientation") {
		t.Fatal("frontier workflow must allow the brief orientation")
	}
	// The sections the profile does not own must be byte-identical to
	// compact — a profile swaps variants, it does not fork the prompt.
	compact := compileBaseSections("/ws", ProfileCompact)
	same := map[string]bool{"identity": true, "skills": true, "memory-policy": true, "delegation": true, "asking": true, "tasks": true, "coding-policy": true, "safety": true}
	compactByID := map[string]string{}
	for _, p := range compact {
		compactByID[p.ID] = p.Content
	}
	for _, p := range parts {
		if same[p.ID] && p.Content != compactByID[p.ID] {
			t.Fatalf("section %s differs between profiles but is not profile-owned", p.ID)
		}
	}
}

// TestCompilePreambleOverrideWinsOverProfile: SystemPromptOverride
// replaces the base wholesale regardless of profile.
func TestCompilePreambleOverrideWinsOverProfile(t *testing.T) {
	parts := compilePreamble("/ws", "", false, openai.ChatMessage{}, false, nil, nil, "custom persona", "", true, ProfileFrontier)
	if len(parts) == 0 || parts[0].ID != "base" || parts[0].Content != "custom persona" {
		t.Fatalf("override must win over the profile, got %+v", parts)
	}
	for _, p := range parts {
		if p.ID == "workflow" || p.ID == "examples" {
			t.Fatalf("profile sections must not appear alongside an override: %s", p.ID)
		}
	}
}
