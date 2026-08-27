package provider

import (
	"encoding/json"
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
		tc := *a.byIndex[i]
		args, ok := validToolCallArguments(tc.Function.Arguments)
		if tc.ID == "" || tc.Function.Name == "" || !ok {
			continue
		}
		tc.Function.Arguments = args
		out = append(out, tc)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validToolCallArguments accepts only the object-shaped JSON every Kram tool
// schema expects. A genuinely zero-argument call is represented by an empty
// stream on some providers, so normalize that one safe case to {}. Truncated
// JSON must never escape the adapter: executing it produces a meaningless
// tool error and persisting it poisons every later provider request.
func validToolCallArguments(arguments string) (string, bool) {
	if arguments == "" {
		return "{}", true
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &object); err != nil || object == nil {
		return "", false
	}
	return arguments, true
}

// sanitizeToolHistory removes protocol-invalid function calls together with
// their orphaned results before a conversation is translated to any upstream
// wire format. It is intentionally provider-wide: a malformed call produced
// by one fallback must not make an otherwise healthy next provider reject the
// entire durable session.
func sanitizeToolHistory(messages []openai.ChatMessage) []openai.ChatMessage {
	validIDs := make(map[string]struct{})
	filteredCalls := make(map[int][]openai.ToolCall)
	for i, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		for _, tc := range message.ToolCalls {
			args, ok := validToolCallArguments(tc.Function.Arguments)
			if tc.ID == "" || tc.Function.Name == "" || !ok {
				continue
			}
			tc.Function.Arguments = args
			filteredCalls[i] = append(filteredCalls[i], tc)
			validIDs[tc.ID] = struct{}{}
		}
	}

	// Which tool_call IDs actually have a matching tool response anywhere
	// in the history? Anything left unanswered is an orphaned call — a
	// crash or panic persisted the assistant's tool-call message but never
	// its results (see agent.runLoop: the assistant message is appended
	// before the tools run). Every OpenAI-compatible API rejects a
	// tool_call with no following tool message as a 400
	// (ClassInvalidRequest, non-retryable), which would brick the session
	// on every subsequent turn — so we synthesize a placeholder result to
	// keep the conversation usable.
	answered := make(map[string]struct{})
	for _, message := range messages {
		if message.Role == "tool" {
			if _, ok := validIDs[message.ToolCallID]; ok {
				answered[message.ToolCallID] = struct{}{}
			}
		}
	}

	out := make([]openai.ChatMessage, 0, len(messages))
	for i, message := range messages {
		switch message.Role {
		case "assistant":
			message.ToolCalls = filteredCalls[i]
			if message.Content == "" && len(message.ToolCalls) == 0 && len(message.ProviderItems) == 0 {
				continue
			}
			out = append(out, message)
			// Repair orphaned calls inline, right after the assistant
			// message that made them, so ordering stays valid (each
			// tool_call immediately followed by its result).
			for _, tc := range message.ToolCalls {
				if _, ok := answered[tc.ID]; ok {
					continue
				}
				out = append(out, openai.ChatMessage{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    "[interrupted: this tool result was not recorded — the session was resumed after an interruption]",
				})
				answered[tc.ID] = struct{}{} // guard against a duplicate call id recurring
			}
			continue
		case "tool":
			if _, ok := validIDs[message.ToolCallID]; !ok {
				continue
			}
		}
		out = append(out, message)
	}
	return out
}
