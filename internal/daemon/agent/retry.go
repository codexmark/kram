package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/openai"
)

const (
	// defaultMaxGatewayRounds is Config.MaxGatewayRounds' zero-value
	// default — see that field's doc comment.
	defaultMaxGatewayRounds = 3
	// baseBackoff and maxBackoff bound the exponential-with-jitter wait
	// between Gateway Rounds: round 1's wait starts near baseBackoff,
	// doubling each round after, capped at maxBackoff so a run of
	// MaxGatewayRounds retries can never turn into a multi-minute hang
	// inside one agent turn.
	baseBackoff = 500 * time.Millisecond
	maxBackoff  = 5 * time.Second
)

// backoffWithJitter returns how long to wait before attempting round
// (0-indexed: 0 is the wait before the *second* overall attempt, i.e.
// the first retry). minRetryAfter, when non-zero, is a floor — honoring
// a real Retry-After the upstream sent (via the last GatewayError) takes
// priority over the computed backoff, never less than what the upstream
// itself asked for.
func backoffWithJitter(round int, minRetryAfter time.Duration) time.Duration {
	wait := baseBackoff << round // baseBackoff * 2^round
	if wait > maxBackoff || wait <= 0 {
		wait = maxBackoff
	}
	// +/-20% jitter so a burst of simultaneous callers retrying the same
	// failure mode doesn't re-converge on the same instant.
	jitter := time.Duration(rand.Int63n(int64(wait) / 5)) // rand.Int63n panics on 0 and requires wait > 0
	wait = wait - wait/10 + jitter
	if minRetryAfter > wait {
		return minRetryAfter
	}
	return wait
}

// callModelWithRetry wraps callModel with a bounded number of "Gateway
// Rounds" — a full fresh ranked-candidate pass, not a raw HTTP retry —
// when the whole round fails with a retryable GatewayError. Runs
// entirely inside one call from runLoop's turn loop, so it never
// consumes MaxTurns: no new logical decision by the model happened here,
// just another attempt at the same one. Re-ranking between rounds needs
// no extra plumbing — kram-gateway already re-ranks fresh on every
// ChatCompletion request (see internal/server/chat.go), so a provider
// that tripped open mid-round-1 or came off cooldown is automatically
// reflected in round 2's ranking with zero round-retry-specific gateway
// code.
// sessionID and sentEstimate feed the token-estimate calibrator: on the
// first successful round, the calibrator learns from this call's own real
// prompt_tokens (result.Usage before it's summed with any failed rounds'
// usage below — summing would double-count the prompt across attempts).
func (s *Service) callModelWithRetry(ctx context.Context, sessionID string, sentEstimate int, model string, messages []openai.ChatMessage, toolDefs []openai.Tool, onEvent EventFunc) (gatewayclient.Result, error) {
	var lastErr error
	var failedUsage openai.Usage
	// salvaged accumulates the partial answer text of every failed
	// streaming round (see streamCall's error return). Non-empty salvage
	// changes the retry from "regenerate from zero" to "resume": the next
	// round sends the partial back as the assistant's own words plus a
	// continuation directive, and the final result re-attaches the partial
	// in front of the continuation — so the user's already-streamed text
	// stays exactly what the persisted answer begins with.
	var salvaged strings.Builder
	for round := 0; round < s.cfg.MaxGatewayRounds; round++ {
		if round > 0 {
			var ge *gatewayclient.GatewayError
			var minWait time.Duration
			if errors.As(lastErr, &ge) {
				minWait = ge.RetryAfter
			}
			wait := backoffWithJitter(round-1, minWait)
			notice := fmt.Sprintf("transient gateway failure, retrying (round %d/%d in %s)",
				round+1, s.cfg.MaxGatewayRounds, wait.Round(time.Millisecond))
			switch {
			case salvaged.Len() > 0:
				notice = fmt.Sprintf("stream dropped mid-answer — resuming where it stopped (round %d/%d in %s)",
					round+1, s.cfg.MaxGatewayRounds, wait.Round(time.Millisecond))
			case ge != nil && ge.Cause == openai.ClassRateLimit:
				// Name the real situation: a rate limit isn't a mysterious
				// "gateway failure", and when the provider sent Retry-After
				// the wait shown is exactly what it asked for.
				notice = fmt.Sprintf("provider rate limited — retrying in %s (round %d/%d)",
					wait.Round(time.Millisecond), round+1, s.cfg.MaxGatewayRounds)
			}
			emit(onEvent, Event{Kind: EventNotice, Notice: notice})
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return gatewayclient.Result{}, ctx.Err()
			}
		}

		callMessages := messages
		if salvaged.Len() > 0 {
			callMessages = continuationMessages(messages, salvaged.String())
		}
		result, err := s.callModel(ctx, model, callMessages, toolDefs, onEvent)
		if err == nil {
			if salvaged.Len() == 0 {
				// Calibrate from this successful call's own prompt_tokens,
				// before folding in any failed rounds' usage below. Skipped
				// after a salvage: the continuation prompt is larger than
				// what sentEstimate measured, so the pair would miscalibrate.
				s.calibrator.observe(sessionID, sentEstimate, result.Usage.PromptTokens)
			}
			result.Usage = openai.AddUsage(failedUsage, result.Usage)
			if salvaged.Len() > 0 {
				result.Content = salvaged.String() + result.Content
			}
			return result, nil
		}
		lastErr = err
		if result.Content != "" {
			salvaged.WriteString(result.Content)
		}

		var ge *gatewayclient.GatewayError
		if errors.As(err, &ge) {
			failedUsage = openai.AddUsage(failedUsage, ge.Usage)
		}
		if !errors.As(err, &ge) || !ge.Retryable {
			// Not a GatewayError at all (e.g. the gateway itself is
			// unreachable — a different failure mode retrying won't fix
			// either), or a GatewayError where not even one candidate in
			// the trail was retryable (see openai.FailureClass.Retryable
			// and writeGatewayError's doc comment — this is deliberately
			// "any attempt retryable", not just the last one, since which
			// candidate a ranking happens to try last is an accident of
			// order, not evidence the whole round is hopeless) — fail
			// immediately rather than burning rounds on a request that
			// will fail the same way every time.
			return gatewayclient.Result{}, err
		}
	}
	return gatewayclient.Result{}, lastErr
}

// humanizeGatewayFailure translates a final (post-retry) gateway failure
// into a message a person can act on, instead of the raw upstream body.
// The original error stays wrapped (%w) so errors.As/Is callers are
// unaffected; anything that isn't a typed GatewayError with a cause worth
// translating passes through untouched.
func humanizeGatewayFailure(err error, rounds int) error {
	var ge *gatewayclient.GatewayError
	if !errors.As(err, &ge) {
		return err
	}
	switch ge.Cause {
	case openai.ClassRateLimit:
		if ge.RetryAfter > 0 {
			return fmt.Errorf("provider rate limit hit — it asked to wait %s and %d retries weren't enough; wait a moment and resend (%w)",
				ge.RetryAfter.Round(time.Second), rounds, err)
		}
		return fmt.Errorf("provider rate limit hit — %d retries weren't enough; wait a moment and resend (%w)", rounds, err)
	case openai.ClassAuth:
		return fmt.Errorf("provider rejected the credential — reconnect the account on the accounts screen (%w)", err)
	default:
		return err
	}
}

// continuationNudge tells the model, in its own conversation, exactly how
// to resume an answer whose stream was cut off. A user-role message rather
// than system: mid-conversation user directives are the shape every
// provider follows most reliably.
const continuationNudge = "[Kram stream recovery: your previous answer above was cut off mid-stream by a transport failure. Continue EXACTLY from where it stopped — do not repeat any part of it, do not apologize, do not restart.]"

// continuationMessages appends the salvaged partial as the assistant's own
// message plus the continuation directive. Built fresh (never mutating the
// caller's slice) and used only for the retried call itself — the partial
// is re-attached to the final result's Content instead (see
// callModelWithRetry), so nothing here is ever persisted to the session.
func continuationMessages(messages []openai.ChatMessage, partial string) []openai.ChatMessage {
	out := make([]openai.ChatMessage, 0, len(messages)+2)
	out = append(out, messages...)
	out = append(out, openai.ChatMessage{Role: "assistant", Content: partial})
	out = append(out, openai.ChatMessage{Role: "user", Content: continuationNudge})
	return out
}
