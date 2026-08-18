package router

import (
	"github.com/codexmark/kram/internal/breaker"
	"github.com/codexmark/kram/internal/provider"
	"github.com/codexmark/kram/internal/telemetry"
)

// Candidate is one provider eligible to serve a request, decorated with
// everything a Strategy needs to rank it. By the time a Strategy ever
// sees a Candidate, the hard constraints — circuit breaker state,
// capability requirements — have already been applied (see
// eligibleCandidates); a Strategy only ever ranks providers that are
// actually usable, never scores its way around a real constraint.
type Candidate struct {
	Provider provider.Provider
	Stats    telemetry.ProviderStats
	// HalfOpen is true if this candidate's breaker is in its half-open
	// trial state — still eligible (Allow() said yes), but strategies may
	// want to treat it more cautiously than a fully closed breaker.
	HalfOpen bool
	// Priority is this candidate's 1-indexed declared position in the
	// combo's provider list (1 = declared first) — the input to the
	// "priority" scoring factor and to the priority strategy's ordering.
	Priority int
	// QualityHint is the operator-configured 0..1 signal from
	// config.ProviderConfig.QualityHint (0 if never set) — see
	// DECISIONS.md for why this is never inferred.
	QualityHint float64
}

// eligibleCandidates builds the Candidate list for one request: filters
// combo providers down to those whose circuit breaker currently allows a
// request and, when the request needs tools or images, those that
// actually support it. This is where capability requirements are enforced
// as a hard constraint — a provider without tool support is never merely
// scored low for a tool-using request, it's absent from the candidate set
// scoring never sees (see DECISIONS.md, "Capabilities are a hard
// constraint, not a score").
func eligibleCandidates(providers []provider.Provider, qualityHints map[string]float64, breakers *breaker.Registry, tel *telemetry.Registry, ctx RouteContext) []Candidate {
	snapshot := tel.Snapshot()
	out := make([]Candidate, 0, len(providers))
	for i, p := range providers {
		if !breakers.Allow(p.ID()) {
			continue
		}
		if ctx.NeedsTools && !p.SupportsTools() {
			continue
		}
		if ctx.NeedsImages && !p.SupportsImages() {
			continue
		}
		out = append(out, Candidate{
			Provider:    p,
			Stats:       snapshot[p.ID()],
			HalfOpen:    breakers.IsHalfOpen(p.ID()),
			Priority:    i + 1,
			QualityHint: qualityHints[p.ID()],
		})
	}
	return out
}
