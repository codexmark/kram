package tools

import "strings"

// ToolMetadata is a short, prompt-facing summary of a tool — distinct
// from and shorter than its Description() (which stays in full in the
// wire schema, for once the model has already selected the tool as a
// candidate and needs the complete picture). A tool with no hand-curated
// ToolMetadata still gets one via Registry.ToolMetadata's fallback —
// that's the point: a tool can't vanish from a generated overview just
// because nobody wrote a Summary for it yet, which is exactly the bug
// that motivated this (21 of 38 registered tools were never mentioned
// in the hand-maintained prompt prose — see DECISIONS.md).
type ToolMetadata struct {
	Summary string
	// PreferOver names another tool this one should be reached for
	// instead of, when relevant — e.g. run_background over bash for a
	// dev server. Empty when there's no such competing default.
	PreferOver string
}

// MetadataProvider is implemented by a Tool that wants a hand-curated
// prompt summary instead of the automatic Description()-derived one.
type MetadataProvider interface {
	ToolMetadata() ToolMetadata
}

// ToolMetadata looks up name's hand-curated metadata, or derives a
// fallback from its Description() (first sentence only, to stay short —
// several Description()s run 3-5 sentences, appropriate for the wire
// schema but too long for a one-line prompt overview) if it doesn't
// implement MetadataProvider, isn't registered, or has an empty
// Summary. Never returns a value with an empty Summary for a registered
// tool — every tool gets *some* usable line.
func (r *Registry) ToolMetadata(name string) ToolMetadata {
	t, ok := r.byName[name]
	if !ok {
		return ToolMetadata{}
	}
	if mp, ok := t.(MetadataProvider); ok {
		if md := mp.ToolMetadata(); md.Summary != "" {
			return md
		}
	}
	return ToolMetadata{Summary: firstSentence(t.Description())}
}

// firstSentence returns the text up to and including the first ". " (or
// the whole string if there's no sentence break) — a cheap, good-enough
// truncation for turning a multi-sentence Description() into a one-line
// summary without needing per-tool curation.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}
