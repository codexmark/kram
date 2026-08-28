package agent

import "strings"

// Model/Agent Profile phase (#130): one prompt used to serve gpt-5.5 and
// a local 9B alike. The section architecture (systemprompt.go,
// compileBaseSections) was built so a profile could swap individual
// sections without touching the rest — this file is that phase's
// selection logic.
//
// Keying decision: model NAME patterns, not provider kind. Provider kind
// cannot distinguish a frontier model from a small one — "openai-compat"
// serves both gpt-5.5 and a local qwen — while the configured model name
// is exactly the fact that matters. Classification is conservative on
// purpose: today's prompt (the compact profile) is proven to work for
// every model class, so frontier is an optimization applied only when
// the whole combo is known to afford it, and an unknown model name
// always means compact.

// PromptProfile selects which variant of the base prompt sections a run
// compiles. The zero value is ProfileCompact — existing Configs and the
// standalone daemon keep today's prompt byte-for-byte without opting in.
type PromptProfile string

const (
	// ProfileCompact is today's prompt exactly: short imperative rules
	// with literal trigger words, plus the few-shot examples section —
	// the style Kram's zero-cost small-model fallback chain needs
	// (see systemprompt.go's second design point).
	ProfileCompact PromptProfile = ""
	// ProfileFrontier serves combos made up exclusively of frontier-class
	// models: the examples section is dropped (rules suffice, and its
	// tokens are paid on every call), and the workflow/output sections
	// allow a one-sentence orientation and richer answer structure.
	ProfileFrontier PromptProfile = "frontier"
)

// frontierModelPrefixes matches, case-insensitively, the configured
// model names Kram treats as frontier-class. Deliberately short and
// conservative: a name that matches nothing here classifies as compact,
// which is always safe — the cost of a miss is an unused optimization,
// never a broken small model reading prose it can't follow.
var frontierModelPrefixes = []string{
	"gpt-4", "gpt-5", "o3", "o4",
	"claude-",
	"gemini-1.5-pro", "gemini-2.5-pro", "gemini-3",
	"grok-3", "grok-4",
	"deepseek-v3", "deepseek-r1",
}

func isFrontierModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false // passthrough/unpinned: the upstream model is unknowable
	}
	for _, p := range frontierModelPrefixes {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// ProfileForModels picks the profile for a combo given its providers'
// configured model names. Frontier only when EVERY model classifies as
// frontier: the prompt is one artifact per run while fallback can route
// any call to any provider in the combo, so it must fit the weakest
// candidate that might receive it. An empty list is compact.
func ProfileForModels(models []string) PromptProfile {
	if len(models) == 0 {
		return ProfileCompact
	}
	for _, m := range models {
		if !isFrontierModel(m) {
			return ProfileCompact
		}
	}
	return ProfileFrontier
}
