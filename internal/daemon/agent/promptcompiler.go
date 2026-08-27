package agent

import (
	"encoding/json"
	"strings"

	"github.com/codexmark/kram/internal/daemon/store"
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

// compileBaseSections turns systemprompt.go's named sections into
// PromptParts, in the same order systemPrompt itself joins them — the
// Model/Agent Profile phase's actual deliverable: section ordering as an
// explicit, inspectable property of this list, instead of implicit in one
// template string. Each part's ID matches the section's own name (see
// systemprompt.go's doc comments for what each one owns); all are
// RefreshStatic/Source "builtin" for the same reason "base" was — see
// systemprompt.go's "Cache stability, per section" note.
func compileBaseSections(workspace string) []PromptPart {
	sections := []struct {
		id      string
		content string
	}{
		{"identity", identitySection(workspace)},
		{"workflow", workflowSection},
		{"skills", skillsSection},
		{"memory-policy", memoryPolicySection},
		{"delegation", delegationSection},
		{"asking", askingSection},
		{"tasks", tasksSection},
		{"coding-policy", codingPolicySection},
		{"output", outputSection},
		{"examples", examplesSection},
		{"safety", safetySection},
	}
	parts := make([]PromptPart, len(sections))
	for i, s := range sections {
		parts[i] = PromptPart{ID: s.id, Placement: PlacementPreamble, Refresh: RefreshStatic, Source: "builtin", Content: s.content}
	}
	return parts
}

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

// formatProjectContextContent wraps AGENTS.md/CLAUDE.md's raw text in the
// framing header the model sees — factored out so runLoop's change-
// detection gate (needsFreshInjection, below) compares against the exact
// same bytes this function puts in the preamble part, rather than
// re-deriving the wrap and risking the two drifting apart.
func formatProjectContextContent(projectContext string) string {
	return "Project context (from AGENTS.md/CLAUDE.md):\n\n" + projectContext
}

// projectContextMarkerName/memoryMarkerName tag a persisted store.Message
// as a change-detection breadcrumb for compilePreamble's project-context/
// memory parts — see needsFreshInjection's own doc comment for why this
// exists and lastMarkerContent for how it's read back.
const (
	projectContextMarkerName = "kram:project_context"
	memoryMarkerName         = "kram:memory"
)

// lastMarkerContent scans effective history for the most recent message
// tagged name, mirroring compaction.EffectiveHistory's own "last one wins"
// scan exactly (same package, same pattern, deliberately not reusing that
// function itself since it scans for a different fixed marker). Because
// effective is already the post-compaction-truncation slice
// compaction.EffectiveHistory produces, a marker from before the last
// compaction is naturally absent here without this function needing to
// know anything about compaction itself — that's what makes reusing the
// existing scan shape enough to solve the "was the last injection pruned
// away" half of change detection for free.
func lastMarkerContent(effective []store.Message, name string) (content string, found bool) {
	for i := len(effective) - 1; i >= 0; i-- {
		if effective[i].Role == "system" && effective[i].Name == name {
			return effective[i].Content, true
		}
	}
	return "", false
}

// needsFreshInjection is the actual token-savings mechanism issue #27
// exists for: project-context and memory are expensive to resend as a
// fresh preamble part on every eligible turn/iteration when the content
// hasn't changed since the model already saw it, persisted verbatim in
// this session's own history (see runLoop's AppendMessage calls tagged
// projectContextMarkerName/memoryMarkerName). Returns true (inject) when
// there's no still-effective prior injection, or its content differs from
// freshContent — false (skip; the model already has it from history)
// when an exact match is still within the effective window. See
// lastMarkerContent's doc comment for how compaction pruning the prior
// injection away is handled without any compaction-specific code here.
func needsFreshInjection(effective []store.Message, markerName, freshContent string) bool {
	last, found := lastMarkerContent(effective, markerName)
	return !found || last != freshContent
}

// compilePreamble reproduces what runLoop's preamble block built inline
// before this refactor existed — base identity, the tools overview
// (generated — see compileToolsOverview), background-job guidance
// (generated — see compileBackgroundJobGuidance), project context, and
// memory, in that order, each only included if present. haveProjectContext/
// haveMemory are the caller's fully-resolved decision of whether to
// include a fresh part this call — runLoop passes false there (distinct
// from "there's no project context/memory at all") when needsFreshInjection
// says an identical copy is already persisted in this session's effective
// history, so this function itself stays a pure "given these already-
// decided inputs, these parts" contract with no change-detection logic of
// its own.
// systemPromptOverride is Config.SystemPromptOverride, threaded through
// as its own parameter rather than read off a Service field so this
// function's pure "given these inputs, these parts" contract holds —
// see the rest of this file's existing parameters for the same reason.
// Empty means today's systemPrompt(workspace) output, unchanged; see
// Config.SystemPromptOverride's own doc comment for exactly what it can
// and can't replace.
// envContext is the run-frozen dynamic environment snapshot (#127) —
// date, active combo, git branch/status/commits; empty omits the part.
// haveSkills is whether any skill is installed, decided once per run
// (see runLoop) — false swaps the skills section's check-the-shelf
// trigger for skillsEmptySection so a fresh install doesn't burn a tool
// call finding nothing (#134).
func compilePreamble(workspace, projectContext string, haveProjectContext bool, memoryMsg openai.ChatMessage, haveMemory bool, reg *tools.Registry, toolOrder []string, systemPromptOverride, envContext string, haveSkills bool) []PromptPart {
	var parts []PromptPart
	if systemPromptOverride != "" {
		// Wholesale replacement stays a single "base" part — see
		// Config.SystemPromptOverride's own doc comment for exactly what
		// it can and can't replace; the generated tools overview and
		// background-job guidance below are never suppressed by it.
		parts = append(parts, PromptPart{ID: "base", Placement: PlacementPreamble, Refresh: RefreshStatic, Source: "builtin", Content: systemPromptOverride})
	} else {
		parts = append(parts, compileBaseSections(workspace)...)
		if !haveSkills {
			for i := range parts {
				if parts[i].ID == "skills" {
					parts[i].Content = skillsEmptySection
				}
			}
		}
	}
	if reg != nil {
		parts = append(parts, compileToolsOverview(reg, toolOrder))
		if guidance := compileBackgroundJobGuidance(reg); guidance.Content != "" {
			parts = append(parts, guidance)
		}
	}
	if envContext != "" {
		parts = append(parts, PromptPart{
			ID: "env-context", Placement: PlacementPreamble, Refresh: RefreshRun, Source: "environment",
			Content: "# Environment\n\n" + envContext,
		})
	}
	if haveProjectContext {
		parts = append(parts, PromptPart{
			ID: "project-context", Placement: PlacementPreamble, Refresh: RefreshIteration, Source: "AGENTS.md",
			Content: formatProjectContextContent(projectContext),
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

// compileTurnPostscript reproduces the after-history conditional
// messages, in a fixed order: empty-retry nudge, then the verification
// gate's nudge (#116), then the near-budget soft landing.
func compileTurnPostscript(emptyRetryUsed, verifyNudgePending, nearBudget bool) []PromptPart {
	var parts []PromptPart
	if emptyRetryUsed {
		parts = append(parts, PromptPart{
			ID: "empty-retry-nudge", Placement: PlacementPostHistory, Refresh: RefreshIteration, Source: "runtime",
			Content: "Your previous response was empty. Answer the user directly in plain text now — do not return another empty response.",
		})
	}
	if verifyNudgePending {
		parts = append(parts, PromptPart{
			ID: "verify-nudge", Placement: PlacementPostHistory, Refresh: RefreshIteration, Source: "runtime",
			Content: verifyNudge,
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
