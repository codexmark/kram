package router

import "github.com/codexmark/kram-gateway/internal/openai"

// ScoreFactor and RankedProviderInfo are the router's own names for the
// wire types in internal/openai (AttemptInfo travels over the gateway's
// HTTP boundary, so its shape lives there) — aliased here so router code
// reads naturally without every file needing to know the wire package.
type ScoreFactor = openai.ScoreFactor

// RankedCandidate is one Strategy.Rank() output entry: a candidate plus
// why it landed where it did. Every eligible candidate appears here, not
// just the ones an attempt executor actually calls — fallback can stop
// before reaching a lower-ranked candidate, but the full ranking is still
// useful for explainability (see DECISIONS.md, "the UI never recomputes a
// score").
type RankedCandidate struct {
	Provider Candidate
	// Score is 0..1 for a scoring strategy; strategies that only order
	// (priority, round-robin, prefix-affinity) leave this at 0 and Factors
	// nil — a zero score there is not a claim about quality, just "this
	// strategy doesn't score."
	Score   float64
	Factors []ScoreFactor
	// Reasons are short tags explaining a ranking decision beyond the raw
	// score — "sticky", "last-known-good", "cache-affinity", "explore".
	Reasons []string
}

// ToRankedProviderInfo converts a full ranking to the wire shape sent to
// clients — a pure, lossless projection (provider ID, score, factors,
// reasons), never a place where a score gets invented or adjusted.
func ToRankedProviderInfo(ranked []RankedCandidate) []openai.RankedProviderInfo {
	if len(ranked) == 0 {
		return nil
	}
	out := make([]openai.RankedProviderInfo, len(ranked))
	for i, rc := range ranked {
		out[i] = openai.RankedProviderInfo{
			Provider: rc.Provider.Provider.ID(),
			Score:    rc.Score,
			Factors:  rc.Factors,
			Reasons:  rc.Reasons,
		}
	}
	return out
}
