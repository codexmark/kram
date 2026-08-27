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
	// adapter arms a phase watchdog (internal/provider/timeout.go's
	// newStreamWatchdog) that bounds connect+headers and then every idle
	// stretch between body reads — a byte-level idle detector at the
	// provider's configured timeout, so a fully dead connection still
	// fails there while a slowly-flowing stream never does. This value
	// here is only the *default*: BoundedPeek takes its idle budget as a
	// parameter, because the right budget depends on whether fallback is
	// even possible — see chat.go's peekIdleFor.
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
// idleBudget is how long to wait between events before giving up; a
// non-positive value uses the short streamPeekIdleTimeout default. The
// caller chooses it per attempt: short while further ranked candidates
// remain (give up fast, fall back), long on the last candidate, where
// rejecting buys nothing — there is nobody left to fall back to, so the
// only honest budget is the provider layer's own.
func BoundedPeek(ctx context.Context, src <-chan provider.StreamEvent, idleBudget time.Duration) StreamPeekResult {
	if idleBudget <= 0 {
		idleBudget = streamPeekIdleTimeout
	}
	var buffered []provider.StreamEvent
	noSignalEvents := 0

	idle := time.NewTimer(idleBudget)
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
			idle.Reset(idleBudget)

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
