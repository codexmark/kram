package agent

import (
	"encoding/json"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func TestRouteTraceAccumulatesMultipleModelCalls(t *testing.T) {
	// This is the regression test for the real bug: the loop used to do
	// result.Attempts = callResult.Attempts on every iteration, so a turn
	// with several tool round-trips silently lost every earlier model
	// call's fallback trail.
	var trace RouteTrace

	trace.addCall("default", "smart", []openai.AttemptInfo{
		{Provider: "anthropic", OK: true, Outcome: openai.OutcomeSuccess},
	}, nil)
	trace.addCall("default", "smart", []openai.AttemptInfo{
		{Provider: "anthropic", OK: false, Outcome: openai.OutcomeError},
		{Provider: "gemini", OK: true, Outcome: openai.OutcomeSuccess},
	}, nil)
	trace.addCall("default", "smart", []openai.AttemptInfo{
		{Provider: "gemini", OK: true, Outcome: openai.OutcomeSuccess},
	}, nil)

	if len(trace.Calls) != 3 {
		t.Fatalf("expected 3 accumulated model calls, got %d", len(trace.Calls))
	}
	if trace.Calls[0].Winner != "anthropic" {
		t.Errorf("call 1 winner: got %q, want anthropic", trace.Calls[0].Winner)
	}
	if len(trace.Calls[1].Attempts) != 2 {
		t.Errorf("call 2 should still have both attempts (the failed anthropic try and the successful gemini one), got %d", len(trace.Calls[1].Attempts))
	}
	if trace.Calls[1].Winner != "gemini" {
		t.Errorf("call 2 winner: got %q, want gemini", trace.Calls[1].Winner)
	}
	if trace.Calls[2].Winner != "gemini" {
		t.Errorf("call 3 winner: got %q, want gemini", trace.Calls[2].Winner)
	}
	for i, c := range trace.Calls {
		if c.Index != i+1 {
			t.Errorf("call %d: Index = %d, want %d (1-indexed)", i, c.Index, i+1)
		}
	}
}

func TestRouteTraceComboAndStrategySetOnce(t *testing.T) {
	var trace RouteTrace
	trace.addCall("default", "smart", []openai.AttemptInfo{{Provider: "a", Outcome: openai.OutcomeSuccess}}, nil)
	trace.addCall("default", "smart", []openai.AttemptInfo{{Provider: "b", Outcome: openai.OutcomeSuccess}}, nil)

	if trace.Combo != "default" || trace.Strategy != "smart" {
		t.Errorf("trace-level Combo/Strategy should be set from the first call, got %q/%q", trace.Combo, trace.Strategy)
	}
	// Per-call Combo/Strategy are still populated independently, so a
	// single RouteCall carries its own context even in isolation.
	for _, c := range trace.Calls {
		if c.Combo != "default" || c.Strategy != "smart" {
			t.Errorf("RouteCall %d: Combo/Strategy = %q/%q", c.Index, c.Combo, c.Strategy)
		}
	}
}

func TestRouteTraceRejectedIsNotError(t *testing.T) {
	var trace RouteTrace
	trace.addCall("default", "smart", []openai.AttemptInfo{
		{Provider: "a", OK: false, Outcome: openai.OutcomeRejected, Reason: "empty response"},
		{Provider: "b", OK: false, Outcome: openai.OutcomeError, Reason: "upstream 500"},
		{Provider: "c", OK: true, Outcome: openai.OutcomeSuccess},
	}, nil)

	call := trace.Calls[0]
	if call.Attempts[0].Outcome != openai.OutcomeRejected {
		t.Errorf("attempt 0 should be OutcomeRejected (gate rejection), got %s", call.Attempts[0].Outcome)
	}
	if call.Attempts[1].Outcome != openai.OutcomeError {
		t.Errorf("attempt 1 should be OutcomeError (transport failure), got %s", call.Attempts[1].Outcome)
	}
	if call.Attempts[0].Outcome == call.Attempts[1].Outcome {
		t.Error("a gate rejection and a transport error must remain distinguishable outcomes")
	}
}

func TestRouteTraceWithNoSuccessHasEmptyWinner(t *testing.T) {
	var trace RouteTrace
	trace.addCall("default", "priority", []openai.AttemptInfo{
		{Provider: "a", Outcome: openai.OutcomeError},
		{Provider: "b", Outcome: openai.OutcomeRejected},
	}, nil)
	if trace.Calls[0].Winner != "" {
		t.Errorf("a call where every attempt failed should have an empty Winner, got %q", trace.Calls[0].Winner)
	}
}

func TestRouteTraceJSONSerializationStable(t *testing.T) {
	var trace RouteTrace
	trace.addCall("default", "smart", []openai.AttemptInfo{
		{Provider: "anthropic", OK: true, Outcome: openai.OutcomeSuccess, LatencyMS: 842, Attempt: 1},
	}, []openai.RankedProviderInfo{
		{Provider: "anthropic", Score: 0.91, Reasons: []string{"sticky"}},
	})

	b, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("RouteTrace should serialize cleanly: %v", err)
	}

	var decoded RouteTrace
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("RouteTrace should round-trip through JSON: %v", err)
	}
	if decoded.Combo != trace.Combo || decoded.Strategy != trace.Strategy {
		t.Errorf("round-tripped trace lost Combo/Strategy: got %+v", decoded)
	}
	if len(decoded.Calls) != 1 || decoded.Calls[0].Winner != "anthropic" {
		t.Errorf("round-tripped trace lost call data: got %+v", decoded.Calls)
	}
	if len(decoded.Calls[0].Ranking) != 1 || decoded.Calls[0].Ranking[0].Score != 0.91 {
		t.Errorf("round-tripped trace lost ranking data: got %+v", decoded.Calls[0].Ranking)
	}

	// Stable across repeated marshaling of the same value — no map
	// iteration or other non-deterministic ordering involved.
	b2, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Error("marshaling the same RouteTrace twice produced different JSON")
	}
}

func TestRouteTraceEmptyMarshalsCleanly(t *testing.T) {
	var trace RouteTrace
	b, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("a zero-value RouteTrace should still marshal: %v", err)
	}
	var decoded RouteTrace
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("a zero-value RouteTrace's JSON should unmarshal cleanly: %v", err)
	}
	if len(decoded.Calls) != 0 {
		t.Errorf("expected no calls, got %d", len(decoded.Calls))
	}
}
