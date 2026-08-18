package router

import (
	"testing"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/openai"
)

func TestGateAcceptsByDefault(t *testing.T) {
	g := NewResponseGate(config.ResponseGateConfig{})
	out := g.Evaluate("", nil, false)
	if !out.Accepted {
		t.Errorf("an unconfigured gate should accept everything (v0 behavior), got rejection: %s", out.Reason)
	}
}

func TestGateRejectsEmpty(t *testing.T) {
	g := NewResponseGate(config.ResponseGateConfig{RejectEmpty: true})
	out := g.Evaluate("   ", nil, true)
	if out.Accepted {
		t.Error("expected an empty (whitespace-only) response with no tool calls to be rejected")
	}
}

func TestGateAcceptsNormalText(t *testing.T) {
	g := NewResponseGate(config.ResponseGateConfig{RejectEmpty: true, MinContentLength: 8})
	out := g.Evaluate("here is a real, complete answer to the question", nil, true)
	if !out.Accepted {
		t.Errorf("expected normal text to be accepted, got rejection: %s", out.Reason)
	}
}

func TestGateAcceptsToolCallWithNoText(t *testing.T) {
	g := NewResponseGate(config.ResponseGateConfig{RejectEmpty: true, MinContentLength: 100})
	out := g.Evaluate("", []openai.ToolCall{{ID: "1", Function: openai.ToolCallFunction{Name: "grep"}}}, true)
	if !out.Accepted {
		t.Errorf("a tool-call-only response should be accepted even with no text and a min length configured, got rejection: %s", out.Reason)
	}
}

func TestGateConfiguredMinLength(t *testing.T) {
	g := NewResponseGate(config.ResponseGateConfig{MinContentLength: 50})
	out := g.Evaluate("too short", nil, true)
	if out.Accepted {
		t.Error("expected a response shorter than the configured minimum to be rejected")
	}
}

func TestGateForbiddenSubstring(t *testing.T) {
	g := NewResponseGate(config.ResponseGateConfig{ForbiddenSubstrings: []string{"internal provider error"}})
	out := g.Evaluate("Sorry, internal provider error occurred while processing your request.", nil, true)
	if out.Accepted {
		t.Error("expected a response containing a configured forbidden substring to be rejected")
	}
}

func TestGateRequireTerminalRejectsCutStream(t *testing.T) {
	g := NewResponseGate(config.ResponseGateConfig{RequireTerminal: true})
	out := g.Evaluate("partial content that never finished", nil, false)
	if out.Accepted {
		t.Error("expected a stream with no terminal signal to be rejected when require_terminal is set")
	}
}

func TestGateNeverRejectsLegitimateRefusal(t *testing.T) {
	// A model's real safety/policy refusal is normal, substantial text —
	// the gate must never single it out just because it happens to
	// contain a common word like "cannot" or "sorry" unless the operator
	// explicitly configured that exact phrase as forbidden.
	g := NewResponseGate(config.ResponseGateConfig{RejectEmpty: true, MinContentLength: 8})
	out := g.Evaluate("I can't help with that specific request, but I can help with something related instead.", nil, true)
	if !out.Accepted {
		t.Errorf("a legitimate, substantial refusal must be accepted, not treated as a technical failure, got rejection: %s", out.Reason)
	}
}
