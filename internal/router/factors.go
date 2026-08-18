package router

// neutralFactorValue is what a factor contributes when Kram genuinely has
// no evidence either way for a candidate — an untested provider, a never-
// configured quality hint. Neutral (the midpoint), never optimistic or
// pessimistic: an unproven candidate should compete on the factors that
// do have data, not be arbitrarily boosted or punished on the ones that
// don't. See DECISIONS.md, "Never fabricate telemetry."
const neutralFactorValue = 0.5

// healthFactor derives from circuit breaker state. A fully circuit-open
// candidate never reaches this function at all — eligibleCandidates
// excludes it before scoring runs, because health/breaker state is a hard
// constraint, not something a good score elsewhere should be able to
// outweigh (see DECISIONS.md). This factor only distinguishes among
// candidates that already passed that filter: a half-open candidate (let
// through for one trial, not yet proven again) scores lower than a fully
// closed one.
func healthFactor(c Candidate) float64 {
	if c.HalfOpen {
		return 0.5
	}
	return 1.0
}

// reliabilityFactor is the candidate's real observed success rate from
// telemetry. No requests yet means no evidence, so it's neutral rather
// than penalized for being unproven.
func reliabilityFactor(c Candidate) float64 {
	if c.Stats.Requests == 0 {
		return neutralFactorValue
	}
	return clamp01(c.Stats.SuccessRate)
}

// latencyFactors normalizes latency *relative to the current candidate
// set*, not against a fixed millisecond threshold — there's no single
// "good" latency across every provider/model combination a user might
// configure, so the fastest candidate actually being ranked scores 1.0
// and the slowest scores 0.0, proportionally between. A candidate with no
// latency data yet gets the neutral value and doesn't stretch the range
// for everyone else.
func latencyFactors(candidates []Candidate) map[string]float64 {
	out := make(map[string]float64, len(candidates))
	var min, max int64
	haveAny := false
	for _, c := range candidates {
		if c.Stats.Requests == 0 || c.Stats.AvgLatencyMS <= 0 {
			continue
		}
		if !haveAny || c.Stats.AvgLatencyMS < min {
			min = c.Stats.AvgLatencyMS
		}
		if !haveAny || c.Stats.AvgLatencyMS > max {
			max = c.Stats.AvgLatencyMS
		}
		haveAny = true
	}
	for _, c := range candidates {
		id := c.Provider.ID()
		switch {
		case c.Stats.Requests == 0 || c.Stats.AvgLatencyMS <= 0 || !haveAny:
			out[id] = neutralFactorValue
		case max == min:
			out[id] = 1.0
		default:
			out[id] = 1.0 - float64(c.Stats.AvgLatencyMS-min)/float64(max-min)
		}
	}
	return out
}

// qualityFactor reads an explicit, operator-configured hint
// (config.ProviderConfig.QualityHint) — Kram has no real measured quality
// signal of its own, so this is never inferred or fabricated. An unset
// hint (zero) is treated as neutral, not as the worst possible score.
func qualityFactor(hint float64) float64 {
	if hint <= 0 {
		return neutralFactorValue
	}
	return clamp01(hint)
}

// cacheAffinityFactor reports whether c is the provider prefix-affinity
// hashing would pick for this request among the current candidate set —
// the same real, reproducible mechanism prefixAffinityStrategy itself
// uses (see affinity.go), not a fabricated cache-hit estimate.
func cacheAffinityFactor(ctx RouteContext, c Candidate, candidates []Candidate) float64 {
	if len(candidates) == 0 || ctx.AffinityKey == "" {
		return neutralFactorValue
	}
	offset := int(hashString(ctx.AffinityKey) % uint64(len(candidates)))
	if candidates[offset].Provider.ID() == c.Provider.ID() {
		return 1.0
	}
	return 0.0
}

// priorityFactor rewards a candidate's declared position: first declared
// scores highest, decaying linearly to the last declared. A single-
// candidate combo always scores 1.0 — there's no "relative to what" for
// its position to mean anything else.
func priorityFactor(c Candidate, total int) float64 {
	if total <= 1 {
		return 1.0
	}
	return 1.0 - float64(c.Priority-1)/float64(total-1)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
