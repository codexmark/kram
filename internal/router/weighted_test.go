package router

import (
	"math/rand"
	"testing"

	"github.com/codexmark/kram/internal/telemetry"
)

func candidate(id string, priority int) Candidate {
	return Candidate{Provider: fakeProvider{id: id, tools: true, images: true}, Priority: priority}
}

// onlyWeights returns a complete weights map with every factor zeroed
// except the ones named — used to isolate a single factor's effect on
// ranking in a test. A partial map like {"latency": 1} would NOT isolate
// latency: newWeightedStrategy merges an override into the preset's own
// weights (any factor the override doesn't mention keeps the preset's
// value), which is the real, documented behavior — so an isolation test
// has to zero every other factor explicitly, not rely on a small
// override number being naturally dominant.
func onlyWeights(names ...string) map[string]float64 {
	w := make(map[string]float64, len(factorNames))
	for _, n := range factorNames {
		w[n] = 0
	}
	for _, n := range names {
		w[n] = 1
	}
	return w
}

func TestNormalizeWeightsSumsToOne(t *testing.T) {
	w := normalizeWeights(map[string]float64{"health": 30, "reliability": 20, "latency": 15, "quality": 15, "cache_affinity": 15, "priority": 5})
	var total float64
	for _, name := range factorNames {
		total += w[name]
	}
	if diff := total - 1.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("normalized weights should sum to 1, got %v (total %v)", w, total)
	}
}

func TestNormalizeWeightsZeroFallsBackToEqualSplit(t *testing.T) {
	w := normalizeWeights(nil)
	equal := 1.0 / float64(len(factorNames))
	for _, name := range factorNames {
		if got := w[name]; got != equal {
			t.Errorf("factor %s: got %v, want equal split %v", name, got, equal)
		}
	}
}

func TestNormalizeWeightsNegativeNeverProducesNaN(t *testing.T) {
	w := normalizeWeights(map[string]float64{"health": -5, "reliability": -100, "latency": 0})
	for _, name := range factorNames {
		v := w[name]
		if v != v { // NaN check
			t.Fatalf("factor %s produced NaN", name)
		}
		if v < 0 {
			t.Errorf("factor %s: negative weight %v survived normalization", name, v)
		}
	}
}

func TestWeightedRankOrdersByScore(t *testing.T) {
	s := newWeightedStrategy("smart", strategyOptions{weights: onlyWeights("reliability")})
	s.opts.sticky, s.sticky = false, nil // isolate score ordering from sticky

	good := candidate("good", 1)
	good.Stats = telemetry.ProviderStats{Requests: 10, Failures: 0, SuccessRate: 1.0}
	bad := candidate("bad", 2)
	bad.Stats = telemetry.ProviderStats{Requests: 10, Failures: 9, SuccessRate: 0.1}

	ranked := s.Rank(RouteContext{}, []Candidate{bad, good})
	if ranked[0].Provider.Provider.ID() != "good" {
		t.Fatalf("expected the more reliable candidate to rank first, got order: %s, %s",
			ranked[0].Provider.Provider.ID(), ranked[1].Provider.Provider.ID())
	}
	if ranked[0].Score <= ranked[1].Score {
		t.Errorf("expected good.Score > bad.Score, got %v vs %v", ranked[0].Score, ranked[1].Score)
	}
}

func TestWeightedHealthFactorPrefersClosedBreaker(t *testing.T) {
	s := newWeightedStrategy("smart", strategyOptions{weights: onlyWeights("health")})
	s.opts.sticky, s.sticky = false, nil

	closed := candidate("closed", 1)
	halfOpen := candidate("half-open", 2)
	halfOpen.HalfOpen = true

	ranked := s.Rank(RouteContext{}, []Candidate{halfOpen, closed})
	if ranked[0].Provider.Provider.ID() != "closed" {
		t.Errorf("a fully closed breaker should outrank a half-open one on the health factor, got %s first", ranked[0].Provider.Provider.ID())
	}
}

func TestWeightedLatencyFactorPrefersFaster(t *testing.T) {
	s := newWeightedStrategy("fast", strategyOptions{weights: onlyWeights("latency")})
	s.opts.sticky, s.sticky = false, nil

	slow := candidate("slow", 1)
	slow.Stats = telemetry.ProviderStats{Requests: 5, AvgLatencyMS: 2000}
	fast := candidate("fast", 2)
	fast.Stats = telemetry.ProviderStats{Requests: 5, AvgLatencyMS: 200}

	ranked := s.Rank(RouteContext{}, []Candidate{slow, fast})
	if ranked[0].Provider.Provider.ID() != "fast" {
		t.Errorf("expected the lower-latency candidate to rank first, got %s", ranked[0].Provider.Provider.ID())
	}
}

func TestWeightedNoDataIsNeutralNotPunished(t *testing.T) {
	s := newWeightedStrategy("smart", strategyOptions{weights: onlyWeights("reliability")})
	s.opts.sticky, s.sticky = false, nil

	untested := candidate("untested", 1) // Stats zero value: Requests == 0
	ranked := s.Rank(RouteContext{}, []Candidate{untested})
	for _, f := range ranked[0].Factors {
		if f.Name == "reliability" && f.Value != neutralFactorValue {
			t.Errorf("an untested candidate's reliability factor should be neutral (%v), got %v", neutralFactorValue, f.Value)
		}
	}
}

func TestWeightedCustomWeightsAreRespected(t *testing.T) {
	s := newWeightedStrategy("weighted", strategyOptions{weights: onlyWeights("priority")})
	s.opts.sticky, s.sticky = false, nil

	first := candidate("first", 1)
	second := candidate("second", 2)
	ranked := s.Rank(RouteContext{}, []Candidate{second, first})
	if ranked[0].Provider.Provider.ID() != "first" {
		t.Errorf("with priority as the only weight, the first-declared candidate should win, got %s", ranked[0].Provider.Provider.ID())
	}
}

func TestWeightedPresetsShareOneEngine(t *testing.T) {
	for _, name := range []string{"smart", "quality", "fast", "cheap", "reliable"} {
		s := newWeightedStrategy(name, strategyOptions{})
		if _, ok := interface{}(s).(*weightedStrategy); !ok {
			t.Errorf("preset %q should be built on weightedStrategy", name)
		}
		if s.Name() != name {
			t.Errorf("preset %q: Name() = %q", name, s.Name())
		}
	}
}

func TestStickyKeepsSameWinnerAcrossCalls(t *testing.T) {
	opts := strategyOptions{sticky: true, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)

	a := candidate("a", 2) // declared second, would lose on priority alone
	b := candidate("b", 1) // declared first, would normally win

	ctx := RouteContext{ComboID: "c1", RunKey: "run-1"}
	first := s.Rank(ctx, []Candidate{a, b})
	if first[0].Provider.Provider.ID() != "b" {
		t.Fatalf("sanity check failed: expected b to win on priority before any sticky pin exists, got %s", first[0].Provider.Provider.ID())
	}
	s.RecordOutcome(ctx, "a", true) // "a" actually won this run

	second := s.Rank(ctx, []Candidate{a, b})
	if second[0].Provider.Provider.ID() != "a" {
		t.Errorf("sticky should keep the pinned winner first even though it scores lower, got %s", second[0].Provider.Provider.ID())
	}
	found := false
	for _, r := range second[0].Reasons {
		if r == "sticky" {
			found = true
		}
	}
	if !found {
		t.Error("the sticky winner's RankedCandidate should carry a \"sticky\" reason")
	}
}

func TestStickyDoesNotApplyAcrossDifferentRuns(t *testing.T) {
	opts := strategyOptions{sticky: true, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)

	a, b := candidate("a", 2), candidate("b", 1)
	ctx1 := RouteContext{ComboID: "c1", RunKey: "run-1"}
	s.RecordOutcome(ctx1, "a", true)

	ctx2 := RouteContext{ComboID: "c1", RunKey: "run-2"}
	ranked := s.Rank(ctx2, []Candidate{a, b})
	if ranked[0].Provider.Provider.ID() != "b" {
		t.Errorf("a different run's affinity key should not inherit another run's sticky pin, got %s", ranked[0].Provider.Provider.ID())
	}
}

func TestStickyFailureReleasesThePinForTheNextWinner(t *testing.T) {
	opts := strategyOptions{sticky: true, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)
	ctx := RouteContext{ComboID: "c1", RunKey: "run-1"}

	s.RecordOutcome(ctx, "a", true)
	// "a" then failed and "b" won instead — the executor calls
	// RecordOutcome again with the real winner.
	s.RecordOutcome(ctx, "b", true)

	ranked := s.Rank(ctx, []Candidate{candidate("a", 1), candidate("b", 2)})
	if ranked[0].Provider.Provider.ID() != "b" {
		t.Errorf("the most recent real winner should become the new sticky pin, got %s", ranked[0].Provider.Provider.ID())
	}
}

func TestStickyDisabledMeansNoPinning(t *testing.T) {
	s := newWeightedStrategy("smart", strategyOptions{sticky: false, weights: onlyWeights("priority")})
	ctx := RouteContext{ComboID: "c1", RunKey: "run-1"}
	s.RecordOutcome(ctx, "a", true)

	ranked := s.Rank(ctx, []Candidate{candidate("a", 2), candidate("b", 1)})
	if ranked[0].Provider.Provider.ID() != "b" {
		t.Errorf("with sticky disabled, ranking should ignore any prior winner and follow scoring, got %s", ranked[0].Provider.Provider.ID())
	}
}

func TestLKGPBoostPromotesLastGoodProviderWithoutBeatingHealthGate(t *testing.T) {
	opts := strategyOptions{sticky: false, lkgpBoost: 0.8, weights: onlyWeights("reliability")}
	s := newWeightedStrategy("smart", opts)
	ctx := RouteContext{ComboID: "c1"}

	weak := candidate("weak", 1)
	weak.Stats = telemetry.ProviderStats{Requests: 10, SuccessRate: 0.2}
	strong := candidate("strong", 2)
	strong.Stats = telemetry.ProviderStats{Requests: 10, SuccessRate: 0.9}

	before := s.Rank(ctx, []Candidate{weak, strong})
	if before[0].Provider.Provider.ID() != "strong" {
		t.Fatalf("sanity check: strong should win before any LKGP boost, got %s", before[0].Provider.Provider.ID())
	}

	s.RecordOutcome(ctx, "weak", true)
	after := s.Rank(ctx, []Candidate{weak, strong})
	if after[0].Provider.Provider.ID() != "weak" {
		t.Errorf("a large enough LKGP boost should be able to promote the last-known-good provider, got %s", after[0].Provider.Provider.ID())
	}
}

func TestLKGPNeverAppliesToCircuitOpenProvider(t *testing.T) {
	// A circuit-open provider is excluded before Rank ever sees it — LKGP
	// boost can only ever apply to a candidate already known eligible.
	r, breakers := newTestRouter(t, "smart", "a", "b")
	req := reqWithAffinity("x")
	ranked, ctx, err := r.Rank("default", req, "")
	if err != nil {
		t.Fatal(err)
	}
	r.RecordOutcome("default", ctx, ranked[0].Provider.Provider.ID(), true)

	for i := 0; i < 3; i++ {
		breakers.ReportFailure(ranked[0].Provider.Provider.ID())
	}

	after, _, err := r.Rank("default", req, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, rc := range after {
		if rc.Provider.Provider.ID() == ranked[0].Provider.Provider.ID() {
			t.Error("a circuit-open last-known-good provider must never appear in the ranking at all")
		}
	}
}

func TestExplorationOccasionallyPromotesNonWinner(t *testing.T) {
	opts := strategyOptions{sticky: false, exploration: 1.0, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)
	s.rand = rand.New(rand.NewSource(1)) // deterministic seed for a reproducible test

	first := candidate("first", 1)
	second := candidate("second", 2)
	sawSecondFirst := false
	for i := 0; i < 20; i++ {
		ranked := s.Rank(RouteContext{}, []Candidate{first, second})
		if ranked[0].Provider.Provider.ID() == "second" {
			sawSecondFirst = true
			break
		}
	}
	if !sawSecondFirst {
		t.Error("with exploration=1.0, the normally-losing candidate should be promoted at least once across 20 tries")
	}
}

func TestExplorationNeverFiresWithSingleCandidate(t *testing.T) {
	opts := strategyOptions{exploration: 1.0, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)
	ranked := s.Rank(RouteContext{}, []Candidate{candidate("only", 1)})
	if len(ranked) != 1 || ranked[0].Provider.Provider.ID() != "only" {
		t.Errorf("a single-candidate ranking should be unaffected by exploration, got %+v", ranked)
	}
}

func TestExplorationNeverOverridesSticky(t *testing.T) {
	opts := strategyOptions{sticky: true, exploration: 1.0, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)
	s.rand = rand.New(rand.NewSource(1))

	a := candidate("a", 2) // declared second, loses on priority alone
	b := candidate("b", 1) // declared first, wins on priority alone
	ctx := RouteContext{ComboID: "c1", RunKey: "run-1"}
	s.RecordOutcome(ctx, "a", true) // "a" actually won this run

	for i := 0; i < 20; i++ {
		ranked := s.Rank(ctx, []Candidate{a, b})
		if ranked[0].Provider.Provider.ID() != "a" {
			t.Fatalf("exploration=1.0 displaced the sticky winner on iteration %d, got %s first", i, ranked[0].Provider.Provider.ID())
		}
	}
}

func TestExplorationWorksWithoutStickyPin(t *testing.T) {
	opts := strategyOptions{sticky: true, exploration: 1.0, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)
	s.rand = rand.New(rand.NewSource(1))

	first := candidate("first", 1)
	second := candidate("second", 2)
	ctx := RouteContext{ComboID: "c1", RunKey: "run-1"} // no RecordOutcome yet — no pin exists

	sawSecondFirst := false
	for i := 0; i < 20; i++ {
		ranked := s.Rank(ctx, []Candidate{first, second})
		if ranked[0].Provider.Provider.ID() == "second" {
			sawSecondFirst = true
			break
		}
	}
	if !sawSecondFirst {
		t.Error("exploration should still promote a non-winner before any sticky pin exists")
	}
}

func TestExplorationWorksWhenStickyDisabled(t *testing.T) {
	opts := strategyOptions{sticky: false, exploration: 1.0, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)
	s.rand = rand.New(rand.NewSource(1))

	first := candidate("first", 1)
	second := candidate("second", 2)

	sawSecondFirst := false
	for i := 0; i < 20; i++ {
		ranked := s.Rank(RouteContext{}, []Candidate{first, second})
		if ranked[0].Provider.Provider.ID() == "second" {
			sawSecondFirst = true
			break
		}
	}
	if !sawSecondFirst {
		t.Error("exploration should remain fully available when sticky is disabled for the combo")
	}
}

func TestStickyFallbackRepinsAfterFailure(t *testing.T) {
	// Guards against an overcorrection: Sticky beating exploration must
	// not accidentally make Sticky absolute. A pinned provider that fails
	// still has to yield to fallback, and the real winner can repin.
	opts := strategyOptions{sticky: true, weights: onlyWeights("priority")}
	s := newWeightedStrategy("smart", opts)
	ctx := RouteContext{ComboID: "c1", RunKey: "run-1"}

	s.RecordOutcome(ctx, "a", true)
	// "a" then failed and "b" won the fallback instead.
	s.RecordOutcome(ctx, "b", true)

	ranked := s.Rank(ctx, []Candidate{candidate("a", 1), candidate("b", 2)})
	if ranked[0].Provider.Provider.ID() != "b" {
		t.Errorf("the real winner of the fallback should become the new sticky pin, got %s", ranked[0].Provider.Provider.ID())
	}
}

func TestDeterministicTieBreaking(t *testing.T) {
	// Two candidates with identical stats should rank consistently
	// (stable sort on equal scores), not flap between runs.
	s := newWeightedStrategy("smart", strategyOptions{sticky: false, weights: onlyWeights("priority")})
	a := candidate("a", 1)
	b := candidate("b", 1) // same declared priority -> identical score under a priority-only weight
	for i := 0; i < 10; i++ {
		ranked := s.Rank(RouteContext{}, []Candidate{a, b})
		if ranked[0].Provider.Provider.ID() != "a" {
			t.Fatalf("tie-break should be stable (input order preserved) across repeated calls, got %s first on iteration %d", ranked[0].Provider.Provider.ID(), i)
		}
	}
}
