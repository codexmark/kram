package agent

import "github.com/codexmark/kram/internal/openai"

// RefreshPolicy documents which of the three real cadences a part
// follows — not three arbitrary "stability tiers", but the three that
// actually exist in runLoop today: a value fixed for the Service's
// lifetime, one fixed for a single run (frozen once per user turn, for
// provider prefix-cache reasons — see recentMemoryMessage's doc
// comment), or one re-evaluated on every internal tool-loop iteration
// within that run. Nothing conditions behavior on this in v1; it exists
// so the already-real distinction between systemPrompt (Static),
// AGENTS.md (Iteration — read fresh every loop pass), and memory (Run —
// frozen once per turn) is inspectable instead of implicit in append
// order, and so a future Model/Agent Profile phase has a real
// vocabulary to extend instead of starting from scratch.
type RefreshPolicy int

const (
	RefreshStatic    RefreshPolicy = iota // fixed for the Service's lifetime
	RefreshRun                            // fixed once per runLoop call (per user turn)
	RefreshIteration                      // re-evaluated on every internal tool-loop pass
)

// PromptPlacement is where a part lands relative to conversation
// history — kept as real data on the part, not just encoded in which
// compiler function produced it, so a future inspector (or a later
// phase adding more post-history reminders) has one place to look.
type PromptPlacement int

const (
	PlacementPreamble    PromptPlacement = iota // before history
	PlacementPostHistory                        // after history
)

// PromptPart is one named, addressable unit of what the model is told —
// the first piece of Kram's Instruction IR. Source names where the
// content came from (builtin/AGENTS.md/memory/runtime), for a future
// prompt-inspection view — not consumed by anything yet in v1.
type PromptPart struct {
	ID        string
	Placement PromptPlacement
	Refresh   RefreshPolicy
	Source    string
	Content   string
}

// compilePreamble reproduces exactly what runLoop's preamble block built
// inline before this refactor existed — same three parts, same order,
// same "only include if present" gating.
func compilePreamble(workspace, projectContext string, haveProjectContext bool, memoryMsg openai.ChatMessage, haveMemory bool) []PromptPart {
	parts := []PromptPart{
		{ID: "base", Placement: PlacementPreamble, Refresh: RefreshStatic, Source: "builtin", Content: systemPrompt(workspace)},
	}
	if haveProjectContext {
		parts = append(parts, PromptPart{
			ID: "project-context", Placement: PlacementPreamble, Refresh: RefreshIteration, Source: "AGENTS.md",
			Content: "Project context (from AGENTS.md/CLAUDE.md):\n\n" + projectContext,
		})
	}
	if haveMemory {
		parts = append(parts, PromptPart{
			ID: "memory", Placement: PlacementPreamble, Refresh: RefreshRun, Source: "memory",
			Content: memoryMsg.Content,
		})
	}
	return parts
}

// compileTurnPostscript reproduces the two after-history conditional
// messages, same order (empty-retry nudge before near-budget message).
func compileTurnPostscript(emptyRetryUsed, nearBudget bool) []PromptPart {
	var parts []PromptPart
	if emptyRetryUsed {
		parts = append(parts, PromptPart{
			ID: "empty-retry-nudge", Placement: PlacementPostHistory, Refresh: RefreshIteration, Source: "runtime",
			Content: "Your previous response was empty. Answer the user directly in plain text now — do not return another empty response.",
		})
	}
	if nearBudget {
		parts = append(parts, PromptPart{
			ID: "turn-budget-soft-landing", Placement: PlacementPostHistory, Refresh: RefreshIteration, Source: "runtime",
			Content: "You are at your turn limit for this task. Provide your best final answer now in plain text — no further tool calls.",
		})
	}
	return parts
}

// partsToMessages renders parts as system-role chat messages, in order.
// Every part is "system" in v1 — no Role/Kind field yet. Adding one now
// would be deciding IR-vs-wire-format semantics ahead of any second
// consumer that would justify it; that question belongs to whichever
// phase first needs a non-OpenAI-shaped instruction (a real Model
// Profile adapter), not this one.
func partsToMessages(parts []PromptPart) []openai.ChatMessage {
	out := make([]openai.ChatMessage, len(parts))
	for i, p := range parts {
		out[i] = openai.ChatMessage{Role: "system", Content: p.Content}
	}
	return out
}
