package router

import (
	"math/rand"
	"time"
)

// p2cStrategy implements Power of Two Choices: with many eligible
// candidates, sampling two at random and picking the healthier one to
// lead is nearly as good as scoring the whole set and much cheaper to
// reason about — the rest of the fallback chain still follows in
// declared-priority order behind the winner. Not a distributed load
// balancer, just a cheap two-sample comparison (see DECISIONS.md).
type p2cStrategy struct {
	rand *rand.Rand // test seam: tests substitute a seeded source for determinism
}

func newP2CStrategy() *p2cStrategy {
	return &p2cStrategy{rand: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

func (*p2cStrategy) Name() string { return "p2c" }

func (s *p2cStrategy) Rank(ctx RouteContext, candidates []Candidate) []RankedCandidate {
	out := priorityStrategy{}.Rank(ctx, candidates)
	if len(out) < 2 {
		return out
	}

	i := s.rand.Intn(len(out))
	j := s.rand.Intn(len(out) - 1)
	if j >= i {
		j++ // sample j from the remaining len-1 candidates, distinct from i
	}

	winner := i
	if p2cScore(out[j].Provider) > p2cScore(out[i].Provider) {
		winner = j
	}
	out[winner].Reasons = append(out[winner].Reasons, "p2c")
	return moveToFront(out, winner)
}

// p2cScore is a small, deterministic health/reliability comparison
// between exactly two sampled candidates — not the full weighted engine;
// P2C's entire point is being cheap.
func p2cScore(c Candidate) float64 {
	return healthFactor(c)*0.6 + reliabilityFactor(c)*0.4
}
