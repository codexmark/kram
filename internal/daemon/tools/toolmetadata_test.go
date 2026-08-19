package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeToolWithMetadata implements both Tool and MetadataProvider.
type fakeToolWithMetadata struct{ name string }

func (f fakeToolWithMetadata) Name() string { return f.name }
func (f fakeToolWithMetadata) Description() string {
	return "A long, multi-sentence description. With a second sentence nobody wants in a one-line overview."
}
func (f fakeToolWithMetadata) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (f fakeToolWithMetadata) Execute(context.Context, json.RawMessage) (string, error) {
	return "", nil
}
func (f fakeToolWithMetadata) ToolMetadata() ToolMetadata {
	return ToolMetadata{Summary: "hand-curated summary", PreferOver: "some_other_tool"}
}

// fakeToolWithoutMetadata implements only Tool — no MetadataProvider.
type fakeToolWithoutMetadata struct{ name string }

func (f fakeToolWithoutMetadata) Name() string { return f.name }
func (f fakeToolWithoutMetadata) Description() string {
	return "First sentence of the fallback. Second sentence that should be dropped."
}
func (f fakeToolWithoutMetadata) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (f fakeToolWithoutMetadata) Execute(context.Context, json.RawMessage) (string, error) {
	return "", nil
}

func TestRegistryToolMetadataUsesHandCuratedWhenPresent(t *testing.T) {
	r := &Registry{byName: map[string]Tool{"curated": fakeToolWithMetadata{name: "curated"}}}

	md := r.ToolMetadata("curated")
	if md.Summary != "hand-curated summary" {
		t.Errorf("Summary = %q, want the hand-curated value", md.Summary)
	}
	if md.PreferOver != "some_other_tool" {
		t.Errorf("PreferOver = %q, want the hand-curated value", md.PreferOver)
	}
}

func TestRegistryToolMetadataFallsBackToDescriptionFirstSentence(t *testing.T) {
	r := &Registry{byName: map[string]Tool{"plain": fakeToolWithoutMetadata{name: "plain"}}}

	md := r.ToolMetadata("plain")
	if md.Summary != "First sentence of the fallback." {
		t.Errorf("Summary = %q, want just the first sentence of Description()", md.Summary)
	}
	if md.PreferOver != "" {
		t.Errorf("PreferOver = %q, want empty for a tool with no curated metadata", md.PreferOver)
	}
}

func TestRegistryToolMetadataUnregisteredNameDoesNotPanic(t *testing.T) {
	r := &Registry{byName: map[string]Tool{}}

	md := r.ToolMetadata("does_not_exist")
	if md.Summary != "" || md.PreferOver != "" {
		t.Errorf("expected a zero-value ToolMetadata for an unregistered name, got %+v", md)
	}
}

func TestFirstSentenceHandlesNoSentenceBreak(t *testing.T) {
	if got := firstSentence("no period here at all"); got != "no period here at all" {
		t.Errorf("firstSentence with no break = %q, want the whole string unchanged", got)
	}
}

// TestRealRegistryEveryToolHasAUsableSummary is a live sanity check
// against the actual production tool set (via NewRegistry, not fakes) —
// confirms every real registered tool produces a non-empty summary, so
// none of them would render as a bare "name — " line in the generated
// overview.
func TestRealRegistryEveryToolHasAUsableSummary(t *testing.T) {
	r := NewRegistry(t.TempDir(), nil, nil)
	for _, info := range r.AllTools() {
		md := r.ToolMetadata(info.Name)
		if md.Summary == "" {
			t.Errorf("tool %q has no usable Summary (neither hand-curated nor Description-derived)", info.Name)
		}
	}
}
