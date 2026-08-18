package router

// weightPresets are the named weight distributions behind
// smart/quality/fast/cheap/reliable — five presets over one engine
// (weightedStrategy), not five independent scoring implementations. Raw
// numbers as given in DECISIONS.md; normalizeWeights turns them into
// fractions summing to 1 regardless of what they're written as here.
//
// These are starting points, not dogma — a combo's own strategy_options
// weights override them per-factor, and any factor left unmentioned in a
// custom override keeps the preset's own weight for it.
var weightPresets = map[string]map[string]float64{
	"smart": {
		"health": 30, "reliability": 20, "latency": 15,
		"quality": 15, "cache_affinity": 15, "priority": 5,
	},
	"quality": {
		"quality": 35, "reliability": 25, "health": 20,
		"cache_affinity": 10, "latency": 5, "priority": 5,
	},
	"fast": {
		"latency": 40, "health": 25, "reliability": 20,
		"cache_affinity": 10, "priority": 5,
	},
	"reliable": {
		"reliability": 40, "health": 30, "cache_affinity": 15,
		"latency": 10, "priority": 5,
	},
	// cheap favors declared priority (where a cost-conscious operator
	// would declare their cheapest provider first) and reliability, but
	// still weighs health heavily — a cheap provider that's currently
	// unhealthy shouldn't win just for being cheap. Kram has no real
	// per-token cost signal of its own to score on directly (see
	// DECISIONS.md, "never fabricate telemetry"), so "cheap" is expressed
	// as "prefer what you told me is your cheap option, as long as it's
	// actually working."
	"cheap": {
		"priority": 35, "health": 30, "reliability": 20,
		"latency": 10, "cache_affinity": 5,
	},
	// weighted (the generic, no-preset name) starts from an equal split —
	// normalizeWeights' own fallback — so "strategy: weighted" with a
	// fully custom weights block has no hidden bias toward any factor
	// before the user's own weights apply.
	"weighted": {},
}

func presetWeights(name string) map[string]float64 {
	if p, ok := weightPresets[name]; ok {
		return p
	}
	return weightPresets["smart"]
}
