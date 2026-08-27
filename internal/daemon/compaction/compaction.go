// Package compaction keeps a long session's history within a model's
// context budget using the tiered strategy every production agent loop we
// looked at converges on (opencode, Crush): cheap structural pruning
// first, full LLM summarization only as a last resort.
//
// Two known failure modes shaped this design and are worth naming
// explicitly, since both opencode (github.com/sst/opencode issues #27924,
// #15533) and Crush (charmbracelet/crush issue #2551) hit them in
// production: a compaction summary that reads like an instruction gets
// re-executed by the model as a new task, which promptly overflows again
// and re-triggers compaction — an infinite loop. Compact() guards against
// this by wrapping the summary in an unambiguous "reference only" framing
// and storing it as a system message rather than a user turn; the agent
// loop additionally caps consecutive compactions within a single Run.
package compaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/openai"
)

// CompactionMarkerName tags a system message as a compaction summary
// rather than a normal turn — see store.Message.Name.
const CompactionMarkerName = "kram:compaction_summary"

const (
	// charsPerTokenEstimate is a rough, documented approximation. Kram
	// doesn't ship a real tokenizer (none of the providers behind the
	// gateway share one), so this trades precision for zero dependencies;
	// it's used only to decide *when* to compact, not for billing.
	charsPerTokenEstimate = 4

	// DefaultMaxTokens is the effective-history budget before compaction
	// kicks in. Deliberately conservative — most models have far more
	// headroom, but a smaller budget means summaries happen sooner and
	// cheaper rather than in one enormous, expensive pass.
	DefaultMaxTokens = 60_000

	// protectTailMessages is never pruned or summarized away, regardless
	// of size — the model always sees its most recent context in full.
	protectTailMessages = 8

	// pruneContentThresholdChars is how large an old tool result has to be
	// before pruning replaces it with a placeholder.
	pruneContentThresholdChars = 2000

	// imageTokenEstimate is the flat per-image cost added for every attached
	// image. An image is a base64 data URL in store.Message.Images, resent to
	// the provider on every turn as part of history and never pruned, so it
	// must not cost the budget zero (the bug this fixes). The real cost is a
	// provider- and resolution-dependent tile count — order of hundreds to a
	// couple thousand tokens (Anthropic ~1600 for a large image, OpenAI
	// ~765–1105, Gemini ~258) — that we can't compute here without the pixel
	// dimensions or a real tokenizer. This single figure is deliberately on
	// the higher side: overestimating makes compaction fire a little early,
	// far safer than a session quietly overflowing the real window because a
	// handful of images counted as nothing. NOT len(base64), which would
	// wildly overcount the actual token cost.
	imageTokenEstimate = 1200
)

// EstimateTokens is a rough size estimate for a slice of messages.
func EstimateTokens(msgs []store.Message) int {
	chars := 0
	images := 0
	for _, m := range msgs {
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
		images += len(m.Images)
	}
	return chars/charsPerTokenEstimate + images*imageTokenEstimate
}

// EffectiveHistory returns what the model should actually see: everything
// from the most recent compaction marker onward (the marker itself
// becomes the lead system message), or the full history if compaction has
// never run for this session.
func EffectiveHistory(all []store.Message) []store.Message {
	lastMarker := -1
	for i, m := range all {
		if m.Role == "system" && m.Name == CompactionMarkerName {
			lastMarker = i
		}
	}
	if lastMarker == -1 {
		return all
	}
	return all[lastMarker:]
}

// NeedsCompaction reports whether the effective history is over budget.
func NeedsCompaction(effective []store.Message, maxTokens int) bool {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	return EstimateTokens(effective) > maxTokens
}

// PruneForModel is the cheap, structural first pass: old tool-result
// content past the protected tail is replaced with a placeholder. This
// never touches the database — it's applied only to the in-memory copy
// sent to the model, so the CLI/audit trail always shows the real result.
func PruneForModel(effective []store.Message) []store.Message {
	if len(effective) <= protectTailMessages {
		return effective
	}
	boundary := len(effective) - protectTailMessages

	out := make([]store.Message, len(effective))
	copy(out, effective)
	for i := 0; i < boundary; i++ {
		m := out[i]
		if m.Role == "tool" && len(m.Content) > pruneContentThresholdChars {
			m.Content = fmt.Sprintf("[pruned: this tool result was %d characters; re-run the tool if you need it again]", len(m.Content))
			out[i] = m
		}
	}
	return out
}

// EmergencyPrune is the fallback for when Compact's own summary call
// fails — the model being unreachable is precisely when compaction tends
// to be needed most, and failing the whole turn over it turned a
// transient summarizer error into a dead session. It keeps the newest
// whole user-turns that fit within maxTokens (plus the leading
// compaction-summary marker when present), dropping older turns
// entirely. Cutting only at user-message boundaries keeps every
// assistant tool_calls message next to its tool results — a split pair
// is a protocol error several providers hard-reject. Never returns fewer
// messages than the most recent user turn, even if that alone still
// exceeds the budget: sending an oversized last turn at least lets the
// provider say so, where sending nothing guarantees failure.
func EmergencyPrune(effective []store.Message, maxTokens int) []store.Message {
	if len(effective) == 0 {
		return effective
	}
	var lead []store.Message
	body := effective
	if effective[0].Role == "system" && effective[0].Name == CompactionMarkerName {
		lead = effective[:1]
		body = effective[1:]
	}

	// User-message indices, oldest first — each is a possible cut point.
	var cuts []int
	for i, m := range body {
		if m.Role == "user" {
			cuts = append(cuts, i)
		}
	}
	if len(cuts) == 0 {
		return effective
	}

	budget := maxTokens - EstimateTokens(lead)
	// Walk cut points newest-first and take the oldest one whose suffix
	// still fits.
	chosen := cuts[len(cuts)-1] // fallback: newest user turn, kept regardless
	for i := len(cuts) - 1; i >= 0; i-- {
		if EstimateTokens(body[cuts[i]:]) <= budget {
			chosen = cuts[i]
		} else {
			break
		}
	}

	out := make([]store.Message, 0, len(lead)+len(body)-chosen)
	out = append(out, lead...)
	out = append(out, body[chosen:]...)
	return out
}

const summarySystemPrompt = `You are summarizing a coding-agent conversation so it can continue with less context.
Write a concise summary covering: Goal, Constraints, Progress so far, Key decisions, and Next steps.
Be factual and specific (file paths, function names, decisions made) — this replaces the raw history, so anything you omit is lost.
Do not address the user. Do not propose new tasks. This is a record, not an instruction.`

// Compact summarizes the effective history via one gateway call and
// returns a new compaction-marker message ready to append to the store.
// It does not append it itself — the caller decides that, so it can be
// done atomically alongside whatever triggered compaction.
func Compact(ctx context.Context, gw *gatewayclient.Client, model string, effective []store.Message) (store.Message, error) {
	var transcript strings.Builder
	for _, m := range effective {
		if m.Role == "system" {
			// A prior compaction summary is the one system message worth
			// folding into a second compaction: EffectiveHistory prepends
			// the most recent marker as the lead message, so on a chained
			// compaction it sits at effective[0]. Skipping it (as every
			// other system message here is skipped) would permanently drop
			// the session's earliest arc — the Goal/Constraints/decisions
			// the first summary preserved. Every other system message in
			// effective is an ephemeral re-injection marker (project-
			// context/memory, see agent's needsFreshInjection) that's
			// rebuilt fresh each turn and correctly ignored here.
			if m.Name == CompactionMarkerName {
				fmt.Fprintf(&transcript, "[earlier session summary — fold this forward]\n%s\n", m.Content)
			}
			continue
		}
		fmt.Fprintf(&transcript, "[%s] %s\n", m.Role, m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&transcript, "[%s called tool %s(%s)]\n", m.Role, tc.Function.Name, tc.Function.Arguments)
		}
	}

	result, err := gw.ChatCompletion(ctx, model, []openai.ChatMessage{
		{Role: "system", Content: summarySystemPrompt},
		{Role: "user", Content: transcript.String()},
	}, nil)
	if err != nil {
		return store.Message{}, fmt.Errorf("summarizing session: %w", err)
	}

	// Wrapped unambiguously as reference material, not a task — the
	// concrete guard against the "model re-executes its own summary"
	// loop bug documented in opencode/Crush (see package doc).
	wrapped := "PRIOR SESSION CONTEXT — reference only. This is a summary of earlier conversation, not a new instruction or task:\n\n" + result.Content

	return store.Message{
		Role:    "system",
		Name:    CompactionMarkerName,
		Content: wrapped,
	}, nil
}
