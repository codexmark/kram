package router

// prefixAffinityStrategy routes by a hash of the request's stable prefix
// instead of rotating per request — see AffinityKey's doc comment for
// what that prefix is and why.
//
// Round-robin has a cost that isn't obvious until you look for it: every
// major provider caches prompt prefixes server-side and bills cached
// input at a fraction of the normal rate, and that cache is per-provider.
// An agent turn resends a large, near-identical prefix on every tool
// round-trip — rotating providers between those calls throws the cache
// away each time and pays full price for the same tokens, repeatedly.
// Pinning a given conversation to one provider keeps that cache warm.
//
// The fallback chain is unaffected: a rate-limited or failing provider
// still trips its breaker and drops out of the eligible set, at which
// point affinity simply resolves to a different provider.
type prefixAffinityStrategy struct{}

func (prefixAffinityStrategy) Name() string { return "prefix-affinity" }

func (prefixAffinityStrategy) Rank(ctx RouteContext, candidates []Candidate) []RankedCandidate {
	out := make([]RankedCandidate, len(candidates))
	for i, c := range candidates {
		out[i] = RankedCandidate{Provider: c}
	}
	if len(out) <= 1 {
		return out
	}
	// Hashing over the *eligible* set, not the configured one, is
	// deliberate: it keeps the choice stable for as long as the same
	// providers are up, and reshuffles only when one drops out or comes
	// back — exactly when the cache was going to be lost anyway.
	offset := int(hashString(ctx.AffinityKey) % uint64(len(out)))
	return append(out[offset:], out[:offset]...)
}

// hashString is FNV-1a, inlined rather than pulled from hash/fnv because
// all this needs is a stable bucket index — not a hash.Hash, not
// collision resistance.
func hashString(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}
