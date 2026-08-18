package tools

import "context"

type depthContextKey struct{}

// WithDepth attaches the current subagent nesting depth (0 = the top-level
// conversation) to ctx. delegate_task reads it back to enforce
// maxSpawnDepth without threading a depth parameter through every tool's
// Execute signature — only delegate_task cares about it.
func WithDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, depthContextKey{}, depth)
}

func depthFromContext(ctx context.Context) int {
	d, _ := ctx.Value(depthContextKey{}).(int)
	return d
}
