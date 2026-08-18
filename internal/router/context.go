package router

import (
	"strings"

	"github.com/codexmark/kram-gateway/internal/openai"
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
	// (system messages + first user message — see AffinityKey), used both
	// by the prefix-affinity strategy and as the sticky-pin key: two
	// requests sharing the same key are, with high probability, tool
	// round-trips within the same agent run.
	AffinityKey string
	// NeedsTools and NeedsImages are hard capability constraints: a
	// candidate lacking one is excluded before scoring ever runs (see
	// candidate.go's eligibleCandidates).
	NeedsTools  bool
	NeedsImages bool
}

// NewRouteContext derives a RouteContext from an actual request — the
// only place capability requirements are decided, so eligibleCandidates'
// hard filtering and every Strategy always see the same, real picture of
// what this request needs.
func NewRouteContext(comboID string, req openai.ChatCompletionRequest) RouteContext {
	needsImages := false
	for _, m := range req.Messages {
		if len(m.Images) > 0 {
			needsImages = true
			break
		}
	}
	return RouteContext{
		ComboID:     comboID,
		AffinityKey: AffinityKey(req),
		NeedsTools:  len(req.Tools) > 0,
		NeedsImages: needsImages,
	}
}
