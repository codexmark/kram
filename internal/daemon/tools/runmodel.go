package tools

import "context"

// runModelContextKey carries the gateway combo the current run is using, so
// delegate_task can default a spawned subagent to the *parent run's* combo
// without threading it through every Execute. Mirrors depth.go's WithDepth.
type runModelContextKey struct{}

// WithRunModel attaches the combo the current run routes to.
func WithRunModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, runModelContextKey{}, model)
}

// runModelFromContext returns the current run's combo, or "" if none was
// attached (an older call path, or a test that never set one).
func runModelFromContext(ctx context.Context) string {
	m, _ := ctx.Value(runModelContextKey{}).(string)
	return m
}
