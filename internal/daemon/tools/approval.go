package tools

import "context"

// ApprovalDecision is the user's answer to a policy-gated tool call.
type ApprovalDecision string

const (
	ApprovalOnce   ApprovalDecision = "once"
	ApprovalAlways ApprovalDecision = "always"
	ApprovalDeny   ApprovalDecision = "deny"
)

// Approver asks the user to sign off on a tool call the permission policy
// marked Ask — implemented by agent.Service, injected per-turn via context,
// same pattern ask.go's Asker uses. Deliberately a separate interface from
// Asker: ask_question means "I don't have enough information to proceed";
// an approval means "I know exactly what I want to do, but policy requires
// sign-off first." Conflating the two would make a policy-driven pause look
// to the model (and the user) like the agent being unsure, which it isn't.
type Approver interface {
	Approve(ctx context.Context, toolName, subject string) (ApprovalDecision, error)
}

type approverContextKey struct{}

// WithApprover attaches the approver for the current turn to ctx — called
// by agent.runLoop before executing tools, mirroring WithAsker.
func WithApprover(ctx context.Context, a Approver) context.Context {
	return context.WithValue(ctx, approverContextKey{}, a)
}

func approverFromContext(ctx context.Context) Approver {
	a, _ := ctx.Value(approverContextKey{}).(Approver)
	return a
}
