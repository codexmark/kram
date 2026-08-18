package router

import "sort"

// priorityStrategy orders candidates by their declared position in the
// combo, unchanged, every call — the "" strategy string's v0 behavior
// (what cmd/kram's autodetect picks when a paid provider leads, so its
// prompt cache stays warm across tool round-trips) preserved exactly, now
// expressed as a Strategy.
type priorityStrategy struct{}

func (priorityStrategy) Name() string { return "priority" }

func (priorityStrategy) Rank(_ RouteContext, candidates []Candidate) []RankedCandidate {
	out := make([]RankedCandidate, len(candidates))
	for i, c := range candidates {
		out[i] = RankedCandidate{Provider: c}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Provider.Priority < out[j].Provider.Priority })
	return out
}
