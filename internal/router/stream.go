package router

import (
	"context"
	"strings"
	"time"

	"github.com/codexmark/kram-gateway/internal/provider"
)

// Bounded-peek limits — small and short on purpose: this exists to catch
// a bad attempt *before* committing to it, not to buffer meaningfully
// long output. A real answer's first non-empty delta or tool call arrives
// almost immediately for every provider Kram talks to; a provider that
// can't produce either within this budget is exactly the case fallback
// should catch.
const (
	streamPeekMaxEvents = 8
	streamPeekTimeout   = 5 * time.Second
)

// StreamPeekResult is what BoundedPeek decided after buffering a small
// prefix of a provider's stream.
type StreamPeekResult struct {
	// Committed is true once a meaningful signal was seen — safe to relay
	// to the client from here on.
	Committed bool
	// Reason is set only when !Committed.
	Reason string
	// Buffered holds every event BoundedPeek already consumed from src —
	// the caller must replay these (in order) to its own client before
	// continuing to read further events from src, whether or not it
	// committed (a rejected attempt's buffered events are simply
	// discarded, never replayed).
	Buffered []provider.StreamEvent
}

// BoundedPeek consumes events from src until it sees a meaningful signal
// (a non-empty text delta, or a terminal event carrying tool calls) or
// gives up — an error, a stream that closes with no meaningful content,
// or the peek budget (streamPeekMaxEvents / streamPeekTimeout) running
// out. "Received some bytes" is deliberately not sufficient signal on its
// own: an empty delta, a role-only opening chunk, or a provider that
// sends periodic keepalive pings would otherwise look like progress.
//
// This runs *before* a caller commits to streaming a response onward to
// its own client — once that commitment is made (headers sent), no
// further fallback is possible, which is exactly the problem this
// exists to solve (see DECISIONS.md, "Bounded streaming peek").
func BoundedPeek(ctx context.Context, src <-chan provider.StreamEvent) StreamPeekResult {
	var buffered []provider.StreamEvent
	timer := time.NewTimer(streamPeekTimeout)
	defer timer.Stop()

	for len(buffered) < streamPeekMaxEvents {
		select {
		case evt, ok := <-src:
			if !ok {
				return StreamPeekResult{Reason: "stream closed with no meaningful content", Buffered: buffered}
			}
			buffered = append(buffered, evt)
			if evt.Err != nil {
				return StreamPeekResult{Reason: "error before meaningful content: " + evt.Err.Error(), Buffered: buffered}
			}
			if strings.TrimSpace(evt.Delta) != "" {
				return StreamPeekResult{Committed: true, Buffered: buffered}
			}
			if evt.Done {
				if len(evt.ToolCalls) > 0 {
					return StreamPeekResult{Committed: true, Buffered: buffered}
				}
				return StreamPeekResult{Reason: "stream closed empty", Buffered: buffered}
			}
		case <-timer.C:
			return StreamPeekResult{Reason: "no meaningful content within the peek window", Buffered: buffered}
		case <-ctx.Done():
			return StreamPeekResult{Reason: ctx.Err().Error(), Buffered: buffered}
		}
	}
	return StreamPeekResult{Reason: "peek buffer exhausted without meaningful content", Buffered: buffered}
}
