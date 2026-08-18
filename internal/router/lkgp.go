package router

import "sync"

// lkgpStore tracks each combo's current "last known good provider" — the
// most recent provider that actually completed a request successfully.
// In-memory only, scoped to one gateway process's lifetime — deliberately
// not persisted to disk: the gateway normally starts and stops together
// with the rest of Kram (see DECISIONS.md's "one binary, in-process"
// decision), and LKGP exists to reflect recent reality, not to survive
// indefinitely across restarts. Revisit if real usage shows a gateway
// running standalone long enough for this to matter.
type lkgpStore struct {
	mu      sync.RWMutex
	byCombo map[string]string
}

func newLKGPStore() *lkgpStore { return &lkgpStore{byCombo: make(map[string]string)} }

func (s *lkgpStore) get(comboID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byCombo[comboID]
}

func (s *lkgpStore) set(comboID, providerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byCombo[comboID] = providerID
}

// lkgpStrategy ranks by declared priority, then moves whichever candidate
// is the combo's current last-known-good provider to the front —
// "priority order, but prefer whoever last actually worked." A circuit-
// open LKGP never wins from this: it's excluded from candidates entirely
// before Rank ever runs (see eligibleCandidates), so this can only ever
// boost a candidate that's already known eligible right now.
//
// This is the standalone-strategy form of LKGP. The same concept also
// works as a boost/modifier inside weightedStrategy (see weighted.go) —
// section 7's "LKGP doesn't have to be its own isolated strategy" — both
// share this package's one lkgpStore type.
type lkgpStrategy struct {
	lkgp *lkgpStore
}

func newLKGPStrategy() *lkgpStrategy { return &lkgpStrategy{lkgp: newLKGPStore()} }

func (*lkgpStrategy) Name() string { return "lkgp" }

func (s *lkgpStrategy) Rank(ctx RouteContext, candidates []Candidate) []RankedCandidate {
	out := priorityStrategy{}.Rank(ctx, candidates)
	if good := s.lkgp.get(ctx.ComboID); good != "" {
		for i, rc := range out {
			if rc.Provider.Provider.ID() == good {
				out[i].Reasons = append(out[i].Reasons, "last-known-good")
				out = moveToFront(out, i)
				break
			}
		}
	}
	return out
}

// RecordOutcome implements outcomeRecorder — see strategy.go.
func (s *lkgpStrategy) RecordOutcome(ctx RouteContext, winner string, ok bool) {
	if ok && winner != "" {
		s.lkgp.set(ctx.ComboID, winner)
	}
}

// moveToFront returns a copy of ranked with the element at i moved to
// index 0, preserving the relative order of everything else.
func moveToFront(ranked []RankedCandidate, i int) []RankedCandidate {
	if i <= 0 {
		return ranked
	}
	out := make([]RankedCandidate, 0, len(ranked))
	out = append(out, ranked[i])
	out = append(out, ranked[:i]...)
	out = append(out, ranked[i+1:]...)
	return out
}
