package agent

import (
	"encoding/json"
	"strings"

	"github.com/codexmark/kram/internal/daemon/tools"
	"github.com/codexmark/kram/internal/openai"
)

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

func estimatePromptPartTokens(parts []PromptPart) int {
	chars := 0
	for _, part := range parts {
		chars += len(part.Content)
	}
	return chars / 4
}

func estimateToolDefinitionTokens(defs any) int {
	b, err := json.Marshal(defs)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// Cache stability, per part — extending RefreshPolicy's own cadence
// documentation with what actually matters for upstream prompt-cache
// reuse (see openai.PromptCacheKeyHeader and the CachedPromptTokens/
// EstimatedCostMicros telemetry that now measures its effect). A part
// re-running every tool-loop iteration (see compilePreamble's call site
// in runLoop) is not the same question as whether its *content* changes
// between those runs — the two are conflated easily, so each part below
// states the content question explicitly, not just its RefreshPolicy.
//
//   - "base" (RefreshStatic): content is systemPrompt(workspace) — re-
//     derived fresh on every tool-loop iteration, but workspace never
//     changes for a Service's lifetime, so the string is identical every
//     time. Prefix-stable for the whole lifetime in practice.
//   - "tools-overview" (RefreshStatic, see compileToolsOverview): same
//     re-derived-but-identical shape as "base". Only changes if the
//     registry's VisibleTools() set changes — a tool toggled in settings
//     or a permission-policy edit — which doesn't happen mid-run today.
//     Adding, removing, or reordering a visible tool invalidates cache
//     reuse from this part onward for every request after that point.
//   - "project-context" (RefreshIteration, see compilePreamble): read
//     fresh from AGENTS.md/CLAUDE.md on every tool-loop iteration by
//     design — it's the one part genuinely expected to legitimately
//     differ turn to turn if the file changes mid-run. Sits after "base"
//     and "tools-overview" in the assembled order, so a change here
//     leaves those two reusable but invalidates this part and everything
//     placed after it (memory, then conversation history) for that
//     request.
//   - "memory" (RefreshRun, see recentMemoryMessage): computed once
//     before the tool-loop starts and reused unchanged for every
//     iteration within that run — prefix-stable across a whole run's
//     tool round-trips, the exact property RefreshRun's own doc comment
//     says it exists for. Can differ between separate runs/user turns.
//   - "empty-retry-nudge" / "turn-budget-soft-landing" (RefreshIteration,
//     PlacementPostHistory, see compileTurnPostscript): always the very
//     last part(s) in the message list, after conversation history. Their
//     presence toggling between iterations changes *that* request's tail
//     only — base, tools-overview, project-context, memory, and history
//     all stay exactly as cacheable as they already were; nothing earlier
//     in the sequence is affected by a part appended at the end.

// toolsOverviewHeader/Footer bookend the generated tool list — Footer is
// the "batch independent calls" line that used to close the hand-written
// "# Tools" section; kept here since it's about tool usage generally and
// was always co-located with the list itself.
const (
	toolsOverviewHeader = "# Tools\n"
	toolsOverviewFooter = "\nCall independent tools in the same turn rather than one per turn. Reading three files or grepping three patterns is one batch, not three round-trips."
)

// compileToolsOverview renders one line per tool the model can actually
// be offered right now — reg.VisibleTools(), the same source Definitions()
// (the wire schema) derives from, deliberately not AllTools() (which only
// excludes disabled tools, not ones the permission policy denies
// unconditionally — see VisibleTools' own doc comment for the concrete
// bug that distinction fixes: a Strict-preset "deny *" tool used to get
// announced here with no matching function in the wire schema). A tool
// with hand-curated ToolMetadata (see internal/daemon/tools/
// toolmetadata.go) gets its Summary and, if set, a "(use this instead of
// X)" cross-reference; everything else falls back to a Description()-
// derived summary — still listed, just not yet hand-tuned, which is
// deliberate (see DECISIONS.md: adding PreferOver for more tools is a
// low-risk follow-up whenever real usage shows the same "competes with a
// default habit" pattern, not something to guess at wholesale up front).
//
// reg may be nil (evals/tests that build a Service without a tool
// registry) — returns an empty part in that case, matching how the rest
// of this file degrades gracefully when its inputs are absent.
//
// order curates presentation order only — see tools.OrderToolNames and
// Config.ToolOrder's own doc comment. nil (the common case) preserves
// today's plain alphabetical order exactly.
func compileToolsOverview(reg *tools.Registry, order []string) PromptPart {
	if reg == nil {
		return PromptPart{ID: "tools-overview", Placement: PlacementPreamble, Refresh: RefreshStatic, Source: "builtin"}
	}

	visible := reg.VisibleTools()
	names := make([]string, len(visible))
	for i, t := range visible {
		names[i] = t.Name()
	}
	names = tools.OrderToolNames(names, order)

	var b strings.Builder
	b.WriteString(toolsOverviewHeader)
	for _, name := range names {
		md := reg.ToolMetadata(name)
		line := name + " — " + md.Summary
		if md.PreferOver != "" {
			line += " (use this instead of " + md.PreferOver + ")"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(toolsOverviewFooter)

	return PromptPart{
		ID: "tools-overview", Placement: PlacementPreamble, Refresh: RefreshStatic, Source: "builtin",
		Content: b.String(),
	}
}

// backgroundJobGuidance is the cross-call habit no single tool's own
// Description() can carry: run_background, process_list, process_output,
// and process_kill each explain what they individually do, but nothing
// tells the model how to use several of them together well. Written in
// terms of what Kram's daemon actually supports today — polling, not
// push notification (there is no job-finished event) — see
// compileBackgroundJobGuidance's own doc comment for why this is
// conditional on run_background actually being visible.
const backgroundJobGuidance = `# Background processes

Use run_background, not bash, for anything that keeps running (a dev server, a watcher, a build daemon).
Track the process id it returns.
Do not busy-poll process_output right after starting a job — check it at a natural point (after finishing other independent work, or right before answering if the job's output matters to your answer).
process_kill a job once it is no longer needed rather than leaving it running past the turn.
process_list recovers state if you lose track of which ids are still relevant — there is no notification when a job finishes; you have to check.`

// compileBackgroundJobGuidance returns the background-process cross-call
// guidance, but only when run_background is actually visible to the
// model in this workspace — a deployment where the tool is disabled or
// permission-denied shouldn't be told a workflow it can't use, the same
// reasoning compileToolsOverview already applies per-tool via
// VisibleTools(). reg == nil (evals/tests without a registry) also
// returns an empty part, matching every other part's degrade-gracefully
// behavior in this file.
func compileBackgroundJobGuidance(reg *tools.Registry) PromptPart {
	part := PromptPart{ID: "background-job-guidance", Placement: PlacementPreamble, Refresh: RefreshStatic, Source: "builtin"}
	if reg == nil {
		return part
	}
	for _, t := range reg.VisibleTools() {
		if t.Name() == "run_background" {
			part.Content = backgroundJobGuidance
			return part
		}
	}
	return part
}

// compilePreamble reproduces what runLoop's preamble block built inline
// before this refactor existed — base identity, the tools overview
// (generated — see compileToolsOverview), background-job guidance
// (generated — see compileBackgroundJobGuidance), project context, and
// memory, in that order, each only included if present.
func compilePreamble(workspace, projectContext string, haveProjectContext bool, memoryMsg openai.ChatMessage, haveMemory bool, reg *tools.Registry, toolOrder []string) []PromptPart {
	parts := []PromptPart{
		{ID: "base", Placement: PlacementPreamble, Refresh: RefreshStatic, Source: "builtin", Content: systemPrompt(workspace)},
	}
	if reg != nil {
		parts = append(parts, compileToolsOverview(reg, toolOrder))
		if guidance := compileBackgroundJobGuidance(reg); guidance.Content != "" {
			parts = append(parts, guidance)
		}
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
