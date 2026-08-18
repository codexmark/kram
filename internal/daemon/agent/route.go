package agent

import "github.com/codexmark/kram/internal/openai"

// RouteCall is one model call's full routing story — every attempt made
// while serving it (success, error, or gate-rejected), not just the one
// that eventually won. Ranking (when the combo's strategy scores) is the
// gateway's own full candidate ranking, unchanged — nothing here is
// recomputed, only carried through.
type RouteCall struct {
	Index    int                         `json:"index"` // 1-indexed position within the run
	Combo    string                      `json:"combo"`
	Strategy string                      `json:"strategy"`
	Attempts []openai.AttemptInfo        `json:"attempts"`
	Ranking  []openai.RankedProviderInfo `json:"ranking,omitempty"`
	Winner   string                      `json:"winner,omitempty"`
}

// RouteTrace accumulates every model call's routing story across one
// user turn (one Service.Run). This exists specifically to fix a real
// bug: the loop used to do `result.Attempts = callResult.Attempts` on
// every iteration, so a turn with several tool round-trips silently lost
// every model call's fallback trail except the very last one — a
// multi-step task like "read a file, grep, edit, run tests, answer" would
// only ever show the routing story for "answer," discarding four other
// model calls' worth of real attempts. RunResult.Attempts is kept as-is
// (the last call's trail, for callers that only want the immediate
// footer view); RouteTrace is the new, complete picture.
type RouteTrace struct {
	Combo    string      `json:"combo"`
	Strategy string      `json:"strategy"`
	Calls    []RouteCall `json:"calls"`
}

// addCall appends one model call's routing story. combo/strategy are
// recorded once, from the first call — every call within a run targets
// the same combo, so repeating it per-entry would be noise (Calls[i]'s
// own Combo/Strategy fields still carry it, for a caller that only has
// one RouteCall in isolation).
func (t *RouteTrace) addCall(combo, strategy string, attempts []openai.AttemptInfo, ranking []openai.RankedProviderInfo) RouteCall {
	if t.Combo == "" {
		t.Combo = combo
	}
	if t.Strategy == "" {
		t.Strategy = strategy
	}
	winner := ""
	for _, a := range attempts {
		if a.Outcome == openai.OutcomeSuccess {
			winner = a.Provider
		}
	}
	call := RouteCall{
		Index: len(t.Calls) + 1, Combo: combo, Strategy: strategy,
		Attempts: attempts, Ranking: ranking, Winner: winner,
	}
	t.Calls = append(t.Calls, call)
	return call
}
