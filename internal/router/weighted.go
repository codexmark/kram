package router

import (
	"math/rand"
	"sort"
	"time"
)

// weightedStrategy is the shared scoring engine behind smart, quality,
// fast, cheap, reliable, and a fully custom "weighted" combo — five
// presets of weights over one engine, not five independent
// implementations (see DECISIONS.md, "Presets, not five engines").
type weightedStrategy struct {
	name    string
	weights map[string]float64 // normalized, sums to 1 across factorNames
	opts    strategyOptions

	lkgp   *lkgpStore
	sticky *stickyStore // nil when opts.sticky is false

	// rand is a test seam: production seeds from real time, tests inject
	// a deterministic source so exploration's behavior is reproducible.
	rand *rand.Rand
}

func newWeightedStrategy(name string, opts strategyOptions) *weightedStrategy {
	weights := opts.weights
	if len(weights) == 0 {
		weights = presetWeights(name)
	} else {
		// A custom weights block starts from the preset so any factor the
		// user didn't mention keeps a sensible weight instead of silently
		// becoming zero.
		merged := make(map[string]float64, len(factorNames))
		for k, v := range presetWeights(name) {
			merged[k] = v
		}
		for k, v := range weights {
			merged[k] = v
		}
		weights = merged
	}

	s := &weightedStrategy{
		name: name, weights: normalizeWeights(weights), opts: opts,
		lkgp: newLKGPStore(),
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if opts.sticky {
		s.sticky = newStickyStore()
	}
	return s
}

func (s *weightedStrategy) Name() string { return s.name }

func (s *weightedStrategy) Rank(ctx RouteContext, candidates []Candidate) []RankedCandidate {
	if len(candidates) == 0 {
		return nil
	}

	latency := latencyFactors(candidates)
	ranked := make([]RankedCandidate, len(candidates))
	for i, c := range candidates {
		factors := []ScoreFactor{
			scoredFactor("health", s.weights["health"], healthFactor(c)),
			scoredFactor("reliability", s.weights["reliability"], reliabilityFactor(c)),
			scoredFactor("latency", s.weights["latency"], latency[c.Provider.ID()]),
			scoredFactor("quality", s.weights["quality"], qualityFactor(c.QualityHint)),
			scoredFactor("cache_affinity", s.weights["cache_affinity"], cacheAffinityFactor(ctx, c, candidates)),
			scoredFactor("priority", s.weights["priority"], priorityFactor(c, len(candidates))),
		}
		var base float64
		for _, f := range factors {
			base += f.Contribution
		}

		score := base
		var reasons []string
		if good := s.lkgp.get(ctx.ComboID); good != "" && good == c.Provider.ID() {
			score = clamp01(score + s.opts.lkgpBoost)
			reasons = append(reasons, "last-known-good")
		}
		if cacheAffinityFactor(ctx, c, candidates) == 1.0 {
			reasons = append(reasons, "cache-affinity")
		}

		ranked[i] = RankedCandidate{Provider: c, Score: score, Factors: factors, Reasons: reasons}
	}

	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })

	// Sticky: while the run's pinned winner is still in this eligible
	// set, it leads regardless of score — a hard preference, not a
	// nudge. See DECISIONS.md, "Smart Sticky."
	if s.sticky != nil {
		if pinned := s.sticky.get(ctx.AffinityKey); pinned != "" {
			for i, rc := range ranked {
				if rc.Provider.Provider.ID() == pinned {
					ranked[i].Reasons = append(ranked[i].Reasons, "sticky")
					ranked = moveToFront(ranked, i)
					break
				}
			}
		}
	}

	// Exploration: with small probability, promote a uniformly random
	// eligible candidate instead of the top-ranked one, so non-winners
	// still occasionally get real telemetry. Never overrides sticky
	// (already applied above) and never fires with fewer than two
	// candidates — there's nothing to explore.
	if s.opts.exploration > 0 && len(ranked) > 1 && s.rand.Float64() < s.opts.exploration {
		i := s.rand.Intn(len(ranked))
		ranked[i].Reasons = append(ranked[i].Reasons, "explore")
		ranked = moveToFront(ranked, i)
	}

	return ranked
}

// RecordOutcome implements outcomeRecorder — see strategy.go. Called by
// the attempt executor once a request actually finishes, so sticky/LKGP
// state always reflects who really won, never a prediction.
func (s *weightedStrategy) RecordOutcome(ctx RouteContext, winner string, ok bool) {
	if !ok || winner == "" {
		return
	}
	s.lkgp.set(ctx.ComboID, winner)
	if s.sticky != nil {
		s.sticky.set(ctx.AffinityKey, winner)
	}
}

func scoredFactor(name string, weight, value float64) ScoreFactor {
	value = clamp01(value)
	return ScoreFactor{Name: name, Weight: weight, Value: value, Contribution: weight * value}
}
