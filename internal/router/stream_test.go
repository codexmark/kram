package router

import (
	"context"
	"errors"
	"testing"

	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/provider"
)

func TestBoundedPeekNoBytesFallsBack(t *testing.T) {
	ch := make(chan provider.StreamEvent)
	close(ch) // closed immediately, no events at all
	res := BoundedPeek(context.Background(), ch)
	if res.Committed {
		t.Error("a stream that closes with zero events should not commit")
	}
}

func TestBoundedPeekEmptyStreamFallsBack(t *testing.T) {
	ch := make(chan provider.StreamEvent, 2)
	ch <- provider.StreamEvent{Delta: ""} // an empty delta — not meaningful signal
	ch <- provider.StreamEvent{Done: true}
	close(ch)
	res := BoundedPeek(context.Background(), ch)
	if res.Committed {
		t.Error("a stream that finishes Done with no text and no tool calls should not commit")
	}
	if len(res.Buffered) != 2 {
		t.Errorf("expected both consumed events to be returned for potential replay/discard, got %d", len(res.Buffered))
	}
}

func TestBoundedPeekErrorBeforeMeaningfulContentFallsBack(t *testing.T) {
	ch := make(chan provider.StreamEvent, 1)
	ch <- provider.StreamEvent{Err: errors.New("upstream reset")}
	close(ch)
	res := BoundedPeek(context.Background(), ch)
	if res.Committed {
		t.Error("an error before any meaningful content must not commit")
	}
	if res.Reason == "" {
		t.Error("expected a non-empty rejection reason")
	}
}

func TestBoundedPeekFirstTextDeltaCommits(t *testing.T) {
	ch := make(chan provider.StreamEvent, 3)
	ch <- provider.StreamEvent{Delta: ""} // role-only opening chunk, not meaningful
	ch <- provider.StreamEvent{Delta: "Hello"}
	ch <- provider.StreamEvent{Done: true}
	res := BoundedPeek(context.Background(), ch)
	if !res.Committed {
		t.Fatalf("a real text delta should commit, got rejection: %s", res.Reason)
	}
	if len(res.Buffered) != 2 {
		t.Errorf("expected exactly the two consumed events (empty delta + the committing one) to be buffered, got %d", len(res.Buffered))
	}
}

func TestBoundedPeekToolCallCommits(t *testing.T) {
	ch := make(chan provider.StreamEvent, 1)
	ch <- provider.StreamEvent{Done: true, ToolCalls: []openai.ToolCall{{ID: "1", Function: openai.ToolCallFunction{Name: "grep"}}}}
	res := BoundedPeek(context.Background(), ch)
	if !res.Committed {
		t.Fatalf("a terminal event carrying tool calls should commit, got rejection: %s", res.Reason)
	}
}

func TestBoundedPeekBufferIsActuallyBounded(t *testing.T) {
	ch := make(chan provider.StreamEvent, streamPeekMaxEvents+5)
	for i := 0; i < streamPeekMaxEvents+5; i++ {
		ch <- provider.StreamEvent{Delta: ""} // never meaningful, keeps peeking
	}
	close(ch)
	res := BoundedPeek(context.Background(), ch)
	if res.Committed {
		t.Error("an endless stream of non-meaningful deltas should never commit")
	}
	if len(res.Buffered) > streamPeekMaxEvents {
		t.Errorf("BoundedPeek buffered %d events, exceeding its own bound of %d", len(res.Buffered), streamPeekMaxEvents)
	}
}

func TestBoundedPeekContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan provider.StreamEvent) // never sends anything
	res := BoundedPeek(ctx, ch)
	if res.Committed {
		t.Error("a canceled context should never result in a commit")
	}
}

// TestBoundedPeekReasoningDoesNotCountAgainstMaxEvents reproduces the real
// bug found live against OpenRouter's gpt-oss and nemotron models: a
// reasoning-capable model sends many chain-of-thought fragments before
// any real answer content. Before this fix, each fragment counted as a
// "no signal" event and exhausted streamPeekMaxEvents long before the
// real content ever arrived — this asserts that no longer happens.
func TestBoundedPeekReasoningDoesNotCountAgainstMaxEvents(t *testing.T) {
	ch := make(chan provider.StreamEvent, streamPeekMaxEvents*3+1)
	for i := 0; i < streamPeekMaxEvents*3; i++ {
		ch <- provider.StreamEvent{Reasoning: "thinking..."}
	}
	ch <- provider.StreamEvent{Delta: "the real answer"}
	res := BoundedPeek(context.Background(), ch)
	if !res.Committed {
		t.Fatalf("real content after many reasoning fragments should still commit, got rejection: %s", res.Reason)
	}
}

// TestBoundedPeekReasoningOnlyStillFallsBackEventually confirms reasoning
// isn't a way to bypass rejection entirely — a stream that only ever
// reasons and then closes must still fall back, same as any other
// content-free stream.
func TestBoundedPeekReasoningOnlyStillFallsBackEventually(t *testing.T) {
	ch := make(chan provider.StreamEvent, 3)
	ch <- provider.StreamEvent{Reasoning: "thinking one"}
	ch <- provider.StreamEvent{Reasoning: "thinking two"}
	close(ch)
	res := BoundedPeek(context.Background(), ch)
	if res.Committed {
		t.Error("a stream that only ever reasons and then closes should not commit")
	}
}
