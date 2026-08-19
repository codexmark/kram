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
	// events before concluding a provider has stalled — reset on every
	// event received, reasoning included, since any event at all is
	// evidence the provider is still actively producing tokens, not
	// dead. This is the *only* time budget here, deliberately: an
	// earlier version also had a fixed overall ceiling regardless of how
	// much reasoning kept resetting this timer, on the theory that a
	// model reasoning forever without ever answering needed a hard stop.
	// Live traffic proved that assumption actively harmful: a real
	// 120B-class reasoning model (OpenRouter's nemotron) legitimately
	// took longer than that ceiling on real prompts — not stalled, just
	// still thinking — and got rejected mid-answer, forcing fallback onto
	// worse (rate-limited) candidates that then failed too, compounding
	// into a fully failed turn (see DECISIONS.md). A genuine hard ceiling
	// still exists one layer down, at the transport: every provider
	// adapter's http.Client carries its own timeout (120s in
	// internal/provider/openai_compat.go). Note what that ceiling
	// actually bounds, though — Go's http.Client.Timeout covers the
	// *entire* request lifetime (dial, headers, and reading the full
	// response body), not idle gaps between reads; it does not reset each
	// time a byte arrives. So it's a real backstop against a connection
	// that goes fully dead or one that's simply slower overall than
	// 120s — not a per-token idle timer the way this package's own
	// streamPeekIdleTimeout is. A stream that's still genuinely, slowly
	// producing tokens past 120s total would still get cut off there;
	// that's a real, known limit of the current design, not a solved
	// case.
	streamPeekIdleTimeout = 5 * time.Second
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
// or the idle budget below running out. "Received some bytes" is
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
			if evt.ToolCallProgress {
				// Same treatment as Reasoning, and for the same reason: a
				// naive fix that only reset the idle timer (every event
				// already does that, above) without also exempting this
				// from noSignalEvents would still exhaust streamPeekMaxEvents
				// on a long tool-call-only stream — a provider sending its
				// arguments as many small fragments, with no leading text,
				// would still get rejected as "peek buffer exhausted"
				// despite being actively productive the whole time.
				continue
			}
			noSignalEvents++
			if noSignalEvents >= streamPeekMaxEvents {
				return StreamPeekResult{Reason: "peek buffer exhausted without meaningful content", Buffered: buffered}
			}
		case <-idle.C:
			return StreamPeekResult{Reason: "no meaningful content within the peek window", Buffered: buffered}
		case <-ctx.Done():
			return StreamPeekResult{Reason: ctx.Err().Error(), Buffered: buffered}
		}
	}
}
