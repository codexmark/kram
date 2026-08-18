package router

import "sync/atomic"

// roundRobinStrategy rotates the leading candidate on every call — one
// instance per combo, so the cursor is shared across every request that
// combo serves, exactly like the v0 router's per-combo cursor did.
// Appropriate when providers are interchangeable peers and the binding
// constraint is rate limits rather than prompt-cache economics (see
// DECISIONS.md).
type roundRobinStrategy struct {
	cursor uint64 // atomic
}

func (*roundRobinStrategy) Name() string { return "round-robin" }

func (s *roundRobinStrategy) Rank(_ RouteContext, candidates []Candidate) []RankedCandidate {
	out := make([]RankedCandidate, len(candidates))
	for i, c := range candidates {
		out[i] = RankedCandidate{Provider: c}
	}
	if len(out) <= 1 {
		return out
	}
	n := atomic.AddUint64(&s.cursor, 1)
	offset := int(n % uint64(len(out)))
	return append(out[offset:], out[:offset]...)
}
