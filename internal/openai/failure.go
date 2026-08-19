package openai

import (
	"context"
	"errors"
	"net"
)

// FailureClass buckets why one provider attempt failed, so a caller can
// tell "genuinely broken upstream" apart from "Kram sent something the
// provider correctly rejected" apart from "the caller gave up" — three
// situations that used to look identical (an error string) and were
// treated identically (poison the circuit breaker, never worth a retry).
type FailureClass string

const (
	// ClassNetwork is a dial/connection-level failure — no response was
	// ever reached.
	ClassNetwork FailureClass = "network"
	// ClassTimeout is a context deadline or client-side timeout.
	ClassTimeout FailureClass = "timeout"
	// ClassServerError is a 5xx — the upstream reached the request but
	// failed to serve it.
	ClassServerError FailureClass = "server_error"
	// ClassRateLimit is a 429 — the upstream is healthy but out of
	// capacity for this account/window right now.
	ClassRateLimit FailureClass = "rate_limit"
	// ClassAuth is a 401/403 — the configured credential is wrong or
	// revoked. Retrying the same request can never fix this, but the
	// provider genuinely is unusable until a human fixes the credential,
	// so this still counts against the circuit breaker (see
	// CountsAgainstBreaker) even though it's never Retryable.
	ClassAuth FailureClass = "auth"
	// ClassInvalidRequest is a 400/404/422 — usually Kram's own malformed
	// request (a wrong role name, a missing required field, an unknown
	// model), not evidence the provider itself is unhealthy. See
	// CountsAgainstBreaker.
	ClassInvalidRequest FailureClass = "invalid_request"
	// ClassContentRejected is never produced by Classify — it's assigned
	// directly by a caller (internal/server/chat.go) for a
	// ResponseGate/StreamGate rejection or a BoundedPeek timeout, neither
	// of which is a transport failure Classify has any status/error to
	// reason about. Kept in this enum so AttemptInfo.Class has one
	// consistent vocabulary regardless of which layer assigned it.
	ClassContentRejected FailureClass = "content_rejected"
	// ClassCancelled is a context.Canceled — the caller gave up (client
	// disconnected, turn aborted), not evidence the provider is unwell.
	ClassCancelled FailureClass = "cancelled"
	// ClassUnknown is anything Classify can't place — treated
	// conservatively (counts against the breaker, since an unrecognized
	// failure is not evidence of health either).
	ClassUnknown FailureClass = "unknown"
)

// Classify derives a FailureClass from a transport-level failure: the
// upstream HTTP status if one was reached (0 if the request never got a
// response at all), and the error itself for the no-response case. It
// has no opinion about ResponseGate/StreamGate rejections — those are
// not transport failures and must be classified ClassContentRejected
// directly by the caller, never routed through Classify (see
// internal/server/chat.go's markFailure/markRejection split).
func Classify(httpStatus int, err error) FailureClass {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ClassCancelled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return ClassTimeout
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return ClassTimeout
		}
	}

	switch {
	case httpStatus == 0:
		if err != nil {
			return ClassNetwork
		}
		return ClassUnknown
	case httpStatus == 429:
		return ClassRateLimit
	case httpStatus == 401 || httpStatus == 403:
		return ClassAuth
	case httpStatus == 400 || httpStatus == 404 || httpStatus == 422:
		return ClassInvalidRequest
	case httpStatus >= 500:
		return ClassServerError
	default:
		return ClassUnknown
	}
}

// Retryable reports whether a Gateway Round retry (a fresh attempt at the
// whole ranked-candidate list, after a backoff) is ever worth attempting
// for this class. Never true for a class where retrying the identical
// request has no chance of a different outcome.
func (c FailureClass) Retryable() bool {
	switch c {
	case ClassNetwork, ClassTimeout, ClassServerError, ClassRateLimit:
		return true
	default:
		return false
	}
}

// CountsAgainstBreaker reports whether this class is real evidence the
// provider itself is unhealthy, as opposed to evidence Kram sent it
// something it correctly rejected (ClassInvalidRequest) or that the
// caller simply gave up (ClassCancelled). Deliberately broader than
// Retryable: ClassAuth counts against the breaker (the provider really
// is unusable right now) even though retrying the same request can never
// fix it.
func (c FailureClass) CountsAgainstBreaker() bool {
	switch c {
	case ClassInvalidRequest, ClassCancelled:
		return false
	default:
		return true
	}
}
