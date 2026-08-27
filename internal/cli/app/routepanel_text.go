package app

// Route-trace panel strings (Ctrl+R): the empty state, the per-attempt
// "selected" label, and the turn's routing summary. The plural-noun consts
// feed pluralPT, so the format lines place the noun last.
const (
	routeNoCalls = "no routed calls yet this session"

	routeModelCallsLine = "%d model %s"
	routeCallSingular   = "call"
	routeCallPlural     = "calls"

	routeUpstreamLine  = "%d upstream %s"
	routeAttemptPlural = "attempts"
	// routeAttemptSingular ("attempt") is defined in misc_text.go (routebar
	// shares the concept) — reused here rather than redeclared.

	routeProviderTimeSuffix = " total provider time"
	routeAttemptSelected    = "selected"
)
