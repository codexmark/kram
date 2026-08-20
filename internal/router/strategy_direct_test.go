package router

import (
	"math/rand"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/telemetry"
)

func directCandidates(ids ...string) []Candidate {
	out := make([]Candidate, len(ids))
	for i, id := range ids {
		out[i] = Candidate{Provider: fakeProvider{id: id, tools: true, images: true}, Priority: i + 1, Stats: telemetry.ProviderStats{SuccessRate: float64(i+1) / 4, Requests: int64(i + 2)}}
	}
	return out
}

func TestOptionFactorPresetAndStickyEdges(t *testing.T) {
	f, boost, explore := false, 0.0, 0.0
	opts := resolveStrategyOptions(config.StrategyOptions{Sticky: &f, LKGPBoost: &boost, Exploration: &explore, Weights: map[string]float64{"health": 2}})
	if opts.sticky || opts.lkgpBoost != 0 || opts.exploration != 0 || opts.weights["health"] != 2 {
		t.Fatalf("options=%#v", opts)
	}
	defaults := resolveStrategyOptions(config.StrategyOptions{})
	if !defaults.sticky || defaults.lkgpBoost != defaultLKGPBoost || defaults.exploration != defaultExploration {
		t.Fatalf("defaults=%#v", defaults)
	}
	if clamp01(-1) != 0 || clamp01(2) != 1 || clamp01(.4) != .4 || qualityFactor(0) != neutralFactorValue || qualityFactor(2) != 1 {
		t.Fatal("factor clamping")
	}
	if presetWeights("unknown")["health"] != weightPresets["smart"]["health"] || presetWeights("fast")["latency"] == 0 {
		t.Fatal("preset fallback")
	}

	s := newStickyStore()
	if s.get("") != "" || s.get("missing") != "" {
		t.Fatal("empty sticky lookup")
	}
	s.set("", "p")
	s.set("x", "")
	if len(s.entries) != 0 {
		t.Fatal("invalid sticky stored")
	}
	old := time.Now().Add(-time.Hour)
	s.entries["old"] = stickyEntry{provider: "a", lastUsed: old}
	s.entries["new"] = stickyEntry{provider: "b", lastUsed: time.Now()}
	s.evictOldestLocked()
	if _, ok := s.entries["old"]; ok {
		t.Fatal("oldest entry not evicted")
	}
	if s.get("new") != "b" {
		t.Fatal("sticky read")
	}
	for i := 0; i < stickyMaxEntries; i++ {
		s.set(string(rune(1000+i)), "p")
	}
	s.set("overflow", "q")
	if len(s.entries) > stickyMaxEntries {
		t.Fatalf("sticky grew to %d", len(s.entries))
	}
}

func TestStrategyFactoryAndNames(t *testing.T) {
	for _, name := range knownStrategyNames {
		if !validStrategyName(name) {
			t.Fatalf("known strategy rejected: %s", name)
		}
		s := newStrategy(name, strategyOptions{})
		if s.Name() == "" {
			t.Fatalf("empty name for %s", name)
		}
	}
	if !validStrategyName("") || validStrategyName("bogus") {
		t.Fatal("strategy validation mismatch")
	}
	if newStrategy("bogus", strategyOptions{}).Name() != "priority" {
		t.Fatal("unknown strategy must be safe priority fallback")
	}
	if got := unknownStrategyError("c", "bogus").Error(); got == "" {
		t.Fatal("empty diagnostic")
	}
}

func TestStandaloneStrategiesDirectBranches(t *testing.T) {
	cands := directCandidates("a", "b", "c")
	ctx := RouteContext{ComboID: "combo", AffinityKey: "stable"}

	priority := priorityStrategy{}
	if priority.Name() != "priority" || len(priority.Rank(ctx, nil)) != 0 {
		t.Fatal("priority edge behavior")
	}
	unsorted := []Candidate{cands[2], cands[0], cands[1]}
	if got := rankedIDs(priority.Rank(ctx, unsorted)); got != "a,b,c" {
		t.Fatalf("priority=%s", got)
	}

	rr := &roundRobinStrategy{}
	if rr.Name() != "round-robin" || len(rr.Rank(ctx, cands[:1])) != 1 {
		t.Fatal("round robin edge behavior")
	}
	if a, b := rankedIDs(rr.Rank(ctx, cands)), rankedIDs(rr.Rank(ctx, cands)); a == b {
		t.Fatalf("round robin did not rotate: %s", a)
	}

	aff := prefixAffinityStrategy{}
	if aff.Name() != "prefix-affinity" || len(aff.Rank(ctx, nil)) != 0 || hashString("stable") == hashString("different") && "stable" != "different" {
		t.Fatal("affinity edge behavior")
	}

	p2c := &p2cStrategy{rand: rand.New(rand.NewSource(7))}
	if p2c.Name() != "p2c" || len(p2c.Rank(ctx, cands[:1])) != 1 {
		t.Fatal("p2c edge behavior")
	}
	ranked := p2c.Rank(ctx, cands)
	if len(ranked) != 3 || len(ranked[0].Reasons) == 0 || ranked[0].Reasons[0] != "p2c" {
		t.Fatalf("p2c=%#v", ranked)
	}
	if p2cScore(cands[0]) < 0 {
		t.Fatal("invalid score")
	}
}

func TestLKGPRecordsOnlySuccessfulWinners(t *testing.T) {
	s := newLKGPStrategy()
	ctx := RouteContext{ComboID: "combo"}
	cands := directCandidates("a", "b", "c")
	if s.Name() != "lkgp" || rankedIDs(s.Rank(ctx, cands)) != "a,b,c" {
		t.Fatal("initial order")
	}
	s.RecordOutcome(ctx, "b", false)
	s.RecordOutcome(ctx, "", true)
	if rankedIDs(s.Rank(ctx, cands)) != "a,b,c" {
		t.Fatal("failed/empty winner recorded")
	}
	s.RecordOutcome(ctx, "b", true)
	ranked := s.Rank(ctx, cands)
	if rankedIDs(ranked) != "b,a,c" || ranked[0].Reasons[0] != "last-known-good" {
		t.Fatalf("ranked=%#v", ranked)
	}
	if got := moveToFront(ranked, 0); &got[0] != &ranked[0] {
		t.Fatal("index zero should return original slice")
	}
}

func TestRouterIntrospectionAndUnknownBranches(t *testing.T) {
	r, _ := newTestRouter(t, "lkgp", "a", "b")
	combos := r.Combos()
	if len(combos) != 1 || combos[0].ID != "default" || combos[0].Strategy != "lkgp" {
		t.Fatalf("combos=%#v", combos)
	}
	if r.StrategyName("default") != "lkgp" || r.StrategyName("missing") != "" {
		t.Fatal("strategy name lookup")
	}
	if _, _, err := r.Rank("missing", openai.ChatCompletionRequest{}, ""); err == nil {
		t.Fatal("expected unknown combo error")
	}
	gate := r.ResponseGateFor("missing")
	if outcome := gate.Evaluate("", nil, false); !outcome.Accepted {
		t.Fatalf("unknown combo gate should be permissive: %#v", outcome)
	}
	r.RecordOutcome("missing", RouteContext{}, "a", true)
	ctx := RouteContext{ComboID: "default"}
	r.RecordOutcome("default", ctx, "b", true)
	ranked, _, err := r.Rank("default", openai.ChatCompletionRequest{}, "")
	if err != nil || ranked[0].Provider.Provider.ID() != "b" {
		t.Fatalf("recorded lkgp not applied: %#v %v", ranked, err)
	}
	info := ToRankedProviderInfo([]RankedCandidate{{Provider: directCandidates("a")[0], Score: .8, Reasons: []string{"why"}}})
	if len(info) != 1 || info[0].Provider != "a" || info[0].Score != .8 {
		t.Fatalf("info=%#v", info)
	}
	if ToRankedProviderInfo(nil) != nil {
		t.Fatal("nil ranking should remain nil")
	}
}
