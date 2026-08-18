package provider

import (
	"sort"

	"github.com/codexmark/kram/internal/openai"
)

// toolCallAccumulator reassembles OpenAI-style tool-call deltas, which
// arrive fragmented across many SSE chunks and keyed by index (id/name in
// the first fragment, arguments dribbled out a few characters at a time in
// later ones).
type toolCallAccumulator struct {
	byIndex map[int]*openai.ToolCall
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{byIndex: make(map[int]*openai.ToolCall)}
}

func (a *toolCallAccumulator) add(index int, id, name, argsFragment string) {
	tc, ok := a.byIndex[index]
	if !ok {
		tc = &openai.ToolCall{Type: "function"}
		a.byIndex[index] = tc
	}
	if id != "" {
		tc.ID = id
	}
	if name != "" {
		tc.Function.Name = name
	}
	tc.Function.Arguments += argsFragment
}

// finish returns the accumulated calls in index order, or nil if none arrived.
func (a *toolCallAccumulator) finish() []openai.ToolCall {
	if len(a.byIndex) == 0 {
		return nil
	}
	indices := make([]int, 0, len(a.byIndex))
	for i := range a.byIndex {
		indices = append(indices, i)
	}
	sort.Ints(indices)

	out := make([]openai.ToolCall, 0, len(indices))
	for _, i := range indices {
		out = append(out, *a.byIndex[i])
	}
	return out
}
