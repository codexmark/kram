package router

import (
	"context"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/provider"
)

// Bounded-peek limits — small and short on purpose: this exists to catch
// a bad attempt *before* committing to it, not to buffer meaningfully
// long output. A real answer's first non-empty delta or tool call arrives
// almost immediately for every provider Kram talks to; a provider that
// can't produce either within budget is exactly the case fallback should
// catch.
const (
	// streamPeekMaxEvents bounds how many *uninformative* events (no
	// content, no reasoning, not a terminal signal) BoundedPeek absorbs
	// before giving up. Reasoning fragments (see StreamEvent.Reasoning)
	// don't count against this — they're real signal that the provider
	// is working, just not the final answer yet — so a reasoning-heavy
	// model isn't punished for the same budget a genuinely silent
	// provider would exhaust in a handful of empty chunks.
	streamPeekMaxEvents = 8
	// streamPeekIdleTimeout bounds how long BoundedPeek waits between
	// events before concluding a provider has stalled. Reset on every
	// event received — reasoning included — since any event at all is
	// evidence the provider is still actively producing tokens, not
	// dead. Before this reset behavior existed, a reasoning-capable
	// model whose thinking phase alone ran past this timeout was
	// misread as a stalled attempt and dropped from the fallback chain,
	// even though real content was still coming (confirmed live against
	// OpenRouter's gpt-oss and nemotron — see DECISIONS.md).
	streamPeekIdleTimeout = 5 * time.Second
	// streamPeekMaxDuration is the hard overall ceiling regardless of how
	// much reasoning keeps the idle timer resetting — without this, a
	// model that reasons indefinitely without ever answering could hold
	// an attempt open forever, defeating the entire point of a *bounded*
	// peek.
	streamPeekMaxDuration = 30 * time.Second
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
// or one of the peek budgets below running out. "Received some bytes" is
// deliberately not sufficient signal on its own: an empty delta, a
// role-only opening chunk, or a provider that sends periodic keepalive
// pings would otherwise look like progress — reasoning fragments are the
// one exception, treated as real (if not yet committable) progress; see
// streamPeekIdleTimeout.
//
// This runs *before* a caller commits to streaming a response onward to
// its own client — once that commitment is made (headers sent), no
// further fallback is possible, which is exactly the problem this
// exists to solve (see DECISIONS.md, "Bounded streaming peek").
func BoundedPeek(ctx context.Context, src <-chan provider.StreamEvent) StreamPeekResult {
	var buffered []provider.StreamEvent
	noSignalEvents := 0

	deadline := time.NewTimer(streamPeekMaxDuration)
	defer deadline.Stop()
	idle := time.NewTimer(streamPeekIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case evt, ok := <-src:
			if !ok {
				return StreamPeekResult{Reason: "stream closed with no meaningful content", Buffered: buffered}
			}
			buffered = append(buffered, evt)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(streamPeekIdleTimeout)

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
			if strings.TrimSpace(evt.Reasoning) != "" {
				continue // real progress, but not itself committable — keep waiting
			}
			noSignalEvents++
			if noSignalEvents >= streamPeekMaxEvents {
				return StreamPeekResult{Reason: "peek buffer exhausted without meaningful content", Buffered: buffered}
			}
		case <-idle.C:
			return StreamPeekResult{Reason: "no meaningful content within the peek window", Buffered: buffered}
		case <-deadline.C:
			return StreamPeekResult{Reason: "reasoning ran past the overall peek time budget without meaningful content", Buffered: buffered}
		case <-ctx.Done():
			return StreamPeekResult{Reason: ctx.Err().Error(), Buffered: buffered}
		}
	}
}
