package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Delegator runs one subtask to completion in a fresh, isolated child
// session and returns its final answer. Implemented by agent.Service;
// declared here (not imported from the agent package) because agent
// already depends on Registry for its own tool calls — agent importing
// tools importing agent would be a cycle. Registry.SetDelegator wires the
// concrete implementation in after both are constructed.
type Delegator interface {
	RunTask(ctx context.Context, goal, taskContext, model string, depth int) (string, error)
}

// defaultMaxSpawnDepth matches Hermes Agent's default (1 = flat: a
// subagent cannot itself delegate further). Kram has no delegation budget
// beyond this depth check, so keeping it shallow by default is the
// difference between a useful fan-out and an unbounded agent tree.
const defaultMaxSpawnDepth = 1

// defaultMaxConcurrentSubagents bounds how many subtasks in one
// delegate_task call run at once — same default Hermes Agent ships with.
const defaultMaxConcurrentSubagents = 3

type delegateTask struct {
	registry      *Registry
	maxDepth      int
	maxConcurrent int
}

func newDelegateTask(r *Registry) *delegateTask {
	return &delegateTask{registry: r, maxDepth: defaultMaxSpawnDepth, maxConcurrent: defaultMaxConcurrentSubagents}
}

func (t *delegateTask) Name() string { return "delegate_task" }
func (t *delegateTask) Description() string {
	return "Delegate one or more independent, self-contained subtasks to fresh subagents that run in isolation — each starts with zero knowledge of this conversation, seeing only the goal and context you give it (closer to briefing a junior engineer than calling a function). Pass multiple tasks to run them in parallel. Use this to fan out independent research or implementation work, not for anything that depends on this conversation's full context."
}

func (t *delegateTask) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"tasks": {
				"type": "array",
				"minItems": 1,
				"description": "One or more independent subtasks. Each runs in its own isolated subagent with no visibility into this conversation or the other tasks in this batch.",
				"items": {
					"type": "object",
					"properties": {
						"goal": {"type": "string", "description": "What the subagent should accomplish — self-contained; it cannot ask you for clarification mid-task."},
						"context": {"type": "string", "description": "Everything the subagent needs to know (file paths, prior findings, constraints) — it sees nothing else from this conversation."},
						"model": {"type": "string", "description": "Optional gateway combo override for this subagent; defaults to the parent's model."}
					},
					"required": ["goal"]
				}
			}
		},
		"required": ["tasks"]
	}`)
}

type delegateTaskArgs struct {
	Tasks []struct {
		Goal    string `json:"goal"`
		Context string `json:"context"`
		Model   string `json:"model"`
	} `json:"tasks"`
}

func (t *delegateTask) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	depth := depthFromContext(ctx)
	if depth >= t.maxDepth {
		return "error: max subagent nesting depth reached — this subagent cannot delegate further", nil
	}
	if t.registry.delegator == nil {
		return "error: delegation is not available in this context", nil
	}

	var args delegateTaskArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if len(args.Tasks) == 0 {
		return "error: at least one task is required", nil
	}

	results := make([]string, len(args.Tasks))
	sem := make(chan struct{}, t.maxConcurrent)
	var wg sync.WaitGroup
	for i, task := range args.Tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, goal, taskContext, model string) {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := t.registry.delegator.RunTask(ctx, goal, taskContext, model, depth+1)
			if err != nil {
				results[i] = fmt.Sprintf("[subtask failed: %v]", err)
				return
			}
			results[i] = res
		}(i, task.Goal, task.Context, task.Model)
	}
	wg.Wait()

	if len(results) == 1 {
		return results[0], nil
	}
	var out strings.Builder
	for i, r := range results {
		fmt.Fprintf(&out, "--- subagent %d/%d ---\n%s\n\n", i+1, len(results), r)
	}
	return out.String(), nil
}
