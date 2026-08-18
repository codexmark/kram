package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Asker pauses the current turn to get a real answer from the user —
// implemented by agent.Service, injected per-call via context (the same
// pattern depth.go uses) rather than through Registry, since unlike
// delegation this needs the *current turn's* live event channel and
// session ID, not just a fixed dependency wired in once at startup.
type Asker interface {
	Ask(ctx context.Context, question string, options []string) (string, error)
}

type askerContextKey struct{}

// WithAsker attaches the asker for the current turn to ctx — called by
// agent.runLoop before executing tools, mirroring WithDepth.
func WithAsker(ctx context.Context, a Asker) context.Context {
	return context.WithValue(ctx, askerContextKey{}, a)
}

func askerFromContext(ctx context.Context) Asker {
	a, _ := ctx.Value(askerContextKey{}).(Asker)
	return a
}

type askQuestion struct{}

func newAskQuestion() *askQuestion { return &askQuestion{} }

func (t *askQuestion) Name() string { return "ask_question" }
func (t *askQuestion) Description() string {
	return "Ask the user a clarifying question and wait for their answer before continuing — use this when a request is genuinely ambiguous or a decision is the user's to make, not for things you could reasonably infer yourself. Provide a few concrete options when there's a natural short list of choices; omit options for a free-text answer. Blocks the turn until the user responds, so use it sparingly."
}
func (t *askQuestion) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"question": {"type": "string", "description": "The question to ask, phrased clearly enough to answer without more context."},
			"options": {"type": "array", "items": {"type": "string"}, "description": "Optional short list of concrete choices. Omit for a free-text answer."}
		},
		"required": ["question"]
	}`)
}

type askQuestionArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func (t *askQuestion) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	asker := askerFromContext(ctx)
	if asker == nil {
		return "error: asking questions isn't available in this context", nil
	}
	var args askQuestionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err), nil
	}
	if args.Question == "" {
		return "error: question must not be empty", nil
	}
	answer, err := asker.Ask(ctx, args.Question, args.Options)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return answer, nil
}
