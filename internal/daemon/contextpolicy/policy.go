// Package contextpolicy turns Kram's context limits into one explicit plan.
// Fixed prompt/tool-schema cost, conversation history, response headroom and
// tool-output growth all draw from the same model window instead of being
// governed by unrelated constants that can collectively overcommit it.
package contextpolicy

// Action is the cheapest intervention that can bring history under its share
// of the context window.
type Action int

const (
	Keep Action = iota
	Prune
	Compact
)

// Plan is one iteration's concrete allocation. FixedTokens includes the
// system/project/memory preamble, postscript and tool definitions.
type Plan struct {
	MaxTokens             int
	FixedTokens           int
	ResponseReserveTokens int
	HistoryBudgetTokens   int
}

// New allocates one eighth of the model window to the answer, capped at 8k
// tokens. Everything left after fixed prompt cost is the history budget.
func New(maxTokens, fixedTokens int) Plan {
	if maxTokens < 0 {
		maxTokens = 0
	}
	if fixedTokens < 0 {
		fixedTokens = 0
	}
	reserve := maxTokens / 8
	if reserve > 8_000 {
		reserve = 8_000
	}
	history := maxTokens - fixedTokens - reserve
	if history < 0 {
		history = 0
	}
	return Plan{
		MaxTokens: maxTokens, FixedTokens: fixedTokens,
		ResponseReserveTokens: reserve, HistoryBudgetTokens: history,
	}
}

// Action compares the real history and its cheap structurally-pruned form.
func (p Plan) Action(historyTokens, prunedHistoryTokens int) Action {
	if historyTokens <= p.HistoryBudgetTokens {
		return Keep
	}
	if prunedHistoryTokens <= p.HistoryBudgetTokens {
		return Prune
	}
	return Compact
}

// RemainingHistoryTokens reports how much additional history can fit before
// the next iteration needs pruning/compaction.
func (p Plan) RemainingHistoryTokens(historyTokens int) int {
	remaining := p.HistoryBudgetTokens - historyTokens
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ToolOutputBudgetChars converts the remaining token allocation using the
// same chars/4 approximation as compaction and caps it at the producer-level
// aggregate limit supplied by the agent loop.
func (p Plan) ToolOutputBudgetChars(historyTokens, maxChars int) int {
	chars := p.RemainingHistoryTokens(historyTokens) * 4
	if chars > maxChars {
		return maxChars
	}
	return chars
}
