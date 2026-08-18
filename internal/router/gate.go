package router

import (
	"strings"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/openai"
)

// GateOutcome is a ResponseGate's verdict on a fully-received response.
type GateOutcome struct {
	Accepted bool
	Reason   string // set only when !Accepted
}

// ResponseGate decides whether a technically-successful response (no
// transport/HTTP error) is actually good enough to end the fallback
// chain — deterministic, technical checks only. This is never a
// mechanism for finding a model willing to ignore another's legitimate
// refusal: a safety or policy refusal is a valid response and is never
// rejected by anything here. The gate exists for empty/truncated/masked-
// error responses, not to second-guess what a model chose to say (see
// DECISIONS.md, "technical error vs quality rejection").
type ResponseGate struct {
	cfg config.ResponseGateConfig
}

// NewResponseGate builds a gate from a combo's response config. A zero
// value config.ResponseGateConfig accepts everything — gating is opt-in,
// matching v0's behavior of accepting any technically-successful
// response.
func NewResponseGate(cfg config.ResponseGateConfig) *ResponseGate {
	return &ResponseGate{cfg: cfg}
}

// Evaluate checks a fully-buffered (or fully peeked-and-replayed, for
// streaming — see stream.go) response. sawTerminal reports whether the
// provider ever produced a proper finish signal (StreamEvent.Done) before
// its channel closed.
func (g *ResponseGate) Evaluate(content string, toolCalls []openai.ToolCall, sawTerminal bool) GateOutcome {
	if g.cfg.RequireTerminal && !sawTerminal {
		return GateOutcome{Reason: "stream ended without a terminal signal"}
	}

	hasContent := strings.TrimSpace(content) != ""
	hasToolCalls := len(toolCalls) > 0

	if g.cfg.RejectEmpty && !hasContent && !hasToolCalls {
		return GateOutcome{Reason: "empty response"}
	}
	if g.cfg.MinContentLength > 0 && !hasToolCalls && len(content) < g.cfg.MinContentLength {
		return GateOutcome{Reason: "content shorter than the configured minimum"}
	}
	for _, sub := range g.cfg.ForbiddenSubstrings {
		if sub != "" && strings.Contains(content, sub) {
			return GateOutcome{Reason: "response matched a forbidden substring"}
		}
	}
	return GateOutcome{Accepted: true}
}
