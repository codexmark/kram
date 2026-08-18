package router

// factorNames is the fixed set of factors the weighted strategy scores
// on, in the order they're presented for explainability — smart/quality/
// fast/cheap/reliable are all presets over these same six weights, never
// five independent engines (see DECISIONS.md).
var factorNames = []string{"health", "reliability", "latency", "quality", "cache_affinity", "priority"}

// normalizeWeights takes a possibly-partial, possibly-zero, possibly-
// negative weight map and returns a complete, non-negative map over every
// factor in factorNames that sums to 1.0 — or, if every input weight is
// zero/invalid/missing, an equal split across all factors. This is what
// lets a user write
//
//	weights: {health: 30, reliability: 20, latency: 15, quality: 15, cache_affinity: 15, priority: 5}
//
// (summing to 100, not 1) and have it work exactly like fractions summing
// to 1 — and what guarantees a zero or malformed weights block can never
// produce NaN or a score that silently favors nothing.
func normalizeWeights(raw map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(factorNames))
	var total float64
	for _, name := range factorNames {
		w := raw[name]
		if w < 0 {
			w = 0
		}
		out[name] = w
		total += w
	}
	if total <= 0 {
		equal := 1.0 / float64(len(factorNames))
		for _, name := range factorNames {
			out[name] = equal
		}
		return out
	}
	for _, name := range factorNames {
		out[name] /= total
	}
	return out
}
