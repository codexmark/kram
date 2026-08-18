package router

import (
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

// convReq builds a request with a stable system+first-user prefix — the
// same shape a persisted conversation keeps across every later turn, and
// exactly the scenario that used to leak a Sticky pin across runs (see
// the issue this fixes: two turns sharing that prefix used to get the
// same AffinityKey, and Sticky used to key off AffinityKey directly).
func convReq(system, firstUser string) openai.ChatCompletionRequest {
	return openai.ChatCompletionRequest{Messages: []openai.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: firstUser},
	}}
}

func TestRunKeyComesFromRunIDWhenProvided(t *testing.T) {
	req := convReq("you are kram", "inspect this repository")
	ctx := NewRouteContext("c1", req, "run-A")
	if ctx.RunKey != "run-A" {
		t.Errorf("RunKey = %q, want the caller-supplied run ID %q", ctx.RunKey, "run-A")
	}
}

func TestRunKeyDiffersAcrossRunIDsForTheIdenticalPrefix(t *testing.T) {
	// Same system + first user message on both calls — this is exactly
	// the shape that leaked before: turn #2 of a conversation still
	// starts with turn #1's opening message, so AffinityKey alone can't
	// tell the two runs apart.
	req := convReq("you are kram", "inspect this repository")

	ctxA := NewRouteContext("c1", req, "run-A")
	ctxB := NewRouteContext("c1", req, "run-B")

	if ctxA.AffinityKey != ctxB.AffinityKey {
		t.Fatalf("sanity check failed: both requests should share the same AffinityKey, got %q vs %q", ctxA.AffinityKey, ctxB.AffinityKey)
	}
	if ctxA.RunKey == ctxB.RunKey {
		t.Errorf("two different run IDs produced the same RunKey (%q) for an identical prompt prefix — this is the leak the fix addresses", ctxA.RunKey)
	}
}

func TestRunKeyFallsBackToAffinityKeyForGenericCallers(t *testing.T) {
	// A standard OpenAI-compatible client that never sends
	// openai.RunIDHeader gets the old prompt-prefix heuristic rather than
	// losing Sticky altogether — a deliberate, documented compatibility
	// choice (see RouteContext.RunKey's doc comment).
	req := convReq("you are kram", "inspect this repository")
	ctx := NewRouteContext("c1", req, "")
	if ctx.RunKey != ctx.AffinityKey {
		t.Errorf("RunKey = %q, want it to fall back to AffinityKey (%q) when no run ID is supplied", ctx.RunKey, ctx.AffinityKey)
	}
}

func TestAffinityKeyUnaffectedByRunID(t *testing.T) {
	// prefix-affinity / cache-affinity regression guard: the same stable
	// prompt prefix must still hash to the same AffinityKey regardless of
	// which run ID (if any) rides along with it.
	req := convReq("you are kram", "inspect this repository")
	ctxNoRun := NewRouteContext("c1", req, "")
	ctxRunA := NewRouteContext("c1", req, "run-A")
	ctxRunB := NewRouteContext("c1", req, "run-B")

	if ctxNoRun.AffinityKey != ctxRunA.AffinityKey || ctxRunA.AffinityKey != ctxRunB.AffinityKey {
		t.Errorf("AffinityKey changed with the run ID: %q, %q, %q — prefix-affinity/cache-affinity must stay independent of Sticky's run identity",
			ctxNoRun.AffinityKey, ctxRunA.AffinityKey, ctxRunB.AffinityKey)
	}
}
