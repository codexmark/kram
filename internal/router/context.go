package router

import (
	"strings"

	"github.com/codexmark/kram/internal/openai"
)

// AffinityKey identifies a request's stable prompt prefix: the leading
// system messages plus the first user message, which is precisely the
// part that does not change across an agent turn's tool round-trips — the
// growing tail of tool calls and results is deliberately excluded, since
// including it would produce a different key on every round-trip and
// defeat the purpose both prefix-affinity routing and sticky routing use
// this for (see DECISIONS.md). Moved here from internal/server/chat.go —
// it's a routing concept, not an HTTP-handler concern, and sticky routing
// needs the same key.
func AffinityKey(req openai.ChatCompletionRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" {
			b.WriteString(m.Content)
			continue
		}
		if m.Role == "user" {
			b.WriteString(m.Content)
			break
		}
	}
	return b.String()
}

// RouteContext carries per-request information a Strategy needs beyond
// the candidate list itself — everything here is either directly read
// from the incoming request or derived deterministically from it, never
// randomly generated or fabricated.
type RouteContext struct {
	// ComboID is the combo being routed — useful for strategies that keep
	// per-combo state (sticky pins, LKGP).
	ComboID string
	// AffinityKey is a stable hash of the request's non-growing prefix
	// (system messages + first user message — see AffinityKey), used by
	// the prefix-affinity strategy and the cache_affinity scoring factor.
	// It is deliberately NOT used for Sticky — see RunKey — because it
	// stays identical across every later user turn in the same
	// conversation, not just within one run.
	AffinityKey string
	// RunKey identifies one agent run: one user turn plus every tool
	// round-trip it causes. This is what Sticky actually pins to. It
	// comes from the caller-supplied openai.RunIDHeader when present
	// (Kram's own daemon always sends one — see gatewayclient.WithRunID);
	// a generic OpenAI-compatible caller that never sends the header gets
	// a best-effort fallback to AffinityKey instead of losing Sticky
	// altogether, at the cost of the same session-wide leak this field
	// exists to fix for Kram's own traffic (see DECISIONS.md, "Sticky is
	// run-scoped, not session-prefix-scoped").
	RunKey string
	// NeedsTools and NeedsImages are hard capability constraints: a
	// candidate lacking one is excluded before scoring ever runs (see
	// candidate.go's eligibleCandidates).
	NeedsTools  bool
	NeedsImages bool
}

// NewRouteContext derives a RouteContext from an actual request — the
// only place capability requirements are decided, so eligibleCandidates'
// hard filtering and every Strategy always see the same, real picture of
// what this request needs. runID is whatever the caller sent via
// openai.RunIDHeader ("" if it sent nothing), already extracted by the
// HTTP handler — see RouteContext.RunKey for what happens when it's
// empty.
func NewRouteContext(comboID string, req openai.ChatCompletionRequest, runID string) RouteContext {
	needsImages := false
	for _, m := range req.Messages {
		if len(m.Images) > 0 {
			needsImages = true
			break
		}
	}
	affinityKey := AffinityKey(req)
	runKey := runID
	if runKey == "" {
		runKey = affinityKey
	}
	return RouteContext{
		ComboID:     comboID,
		AffinityKey: affinityKey,
		RunKey:      runKey,
		NeedsTools:  len(req.Tools) > 0,
		NeedsImages: needsImages,
	}
}
