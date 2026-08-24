package router

import "fmt"

// Strategy decides which candidates to try and in what order. It always
// returns a full ranking, never just a winner — the existing fallback
// behavior depends on having an ordered list to fall through, and an
// explainability UI depends on seeing where every eligible candidate
// landed, not just the top pick (see DECISIONS.md, "Combos v2").
//
// Rank is called with an already-filtered candidate list: circuit-open
// and capability-incompatible providers are never passed in (see
// candidate.go's eligibleCandidates) — a Strategy implementation never
// needs to re-check those hard constraints itself.
type Strategy interface {
	Name() string
	Rank(ctx RouteContext, candidates []Candidate) []RankedCandidate
}

// newStrategy builds the Strategy a combo's configured name selects.
// Unknown names fall back to priority (declared order) rather than
// erroring — the same forgiving default an empty/unrecognized strategy
// string always had in v0, so an old or slightly-misspelled config keeps
// working instead of refusing to start.
func newStrategy(name string, opts strategyOptions) Strategy {
	switch name {
	case "", "priority":
		return priorityStrategy{}
	case "round-robin":
		return &roundRobinStrategy{}
	case "prefix-affinity":
		return prefixAffinityStrategy{}
	case "smart", "quality", "fast", "cheap", "reliable", "weighted":
		return newWeightedStrategy(name, opts)
	case "lkgp":
		return newLKGPStrategy()
	case "p2c":
		return newP2CStrategy()
	default:
		return priorityStrategy{}
	}
}

// outcomeRecorder is implemented by strategies that keep state between
// requests (sticky pins, last-known-good) and need to learn who actually
// won a request — priority/round-robin/prefix-affinity/p2c don't
// implement it, since they have no such state to update.
type outcomeRecorder interface {
	RecordOutcome(ctx RouteContext, winner string, ok bool)
}

// knownStrategyNames lists every selectable strategy name, for error
// messages and the CLI's route bar label — kept in one place so adding a
// strategy means updating exactly here and in newStrategy.
var knownStrategyNames = []string{
	"priority", "round-robin", "prefix-affinity",
	"smart", "quality", "fast", "cheap", "reliable", "weighted",
	"lkgp", "p2c",
}

// KnownStrategyNames returns the strategies accepted by SetStrategy and the
// config loader. The copy keeps callers from mutating the router's source of
// truth while still letting status/UI surfaces discover the live list.
func KnownStrategyNames() []string {
	return append([]string(nil), knownStrategyNames...)
}

func validStrategyName(name string) bool {
	if name == "" {
		return true
	}
	for _, n := range knownStrategyNames {
		if n == name {
			return true
		}
	}
	return false
}

func unknownStrategyError(comboID, name string) error {
	return fmt.Errorf("combo %q: unknown strategy %q (known: %v)", comboID, name, knownStrategyNames)
}
