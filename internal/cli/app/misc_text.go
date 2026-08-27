package app

// User-facing English strings for the miscellaneous TUI components in this
// package: the route bar, the token-usage footer, the files-touched row, and
// the approval / ask_question option pickers. Centralized for issue #74
// (Brazilian-Portuguese -> English migration).
const (
	// routebar.go — top status strip.
	// routeStrategyPrefix is rendered in two places (the visible block and the
	// clickable-width measurement); it MUST stay a single shared literal so the
	// hit region and the drawn text can never drift apart.
	routeStrategyPrefix  = "strategy:"
	routeEvaluatingFmt   = "evaluating %d upstreams"
	routeRoutesFmt       = "%d routes"
	routeAttemptSingular = "attempt"
	routeAttemptsPlural  = "attempts"

	// footer_helpers.go — token-usage footer labels.
	footerReasoningFmt = " · reasoning %d"
	footerCostFmt      = " · ≈$%.4f"

	// filestouched.go — turn-ending files-touched row.
	filesTouchedLabel       = "files: "
	filesTouchedOverflowFmt = " +%d more"

	// approval.go / ask.go — footer hints for the option pickers.
	approvalHint    = "↑↓ choose · enter confirm"
	askQuestionHint = "↑↓ choose · enter answer"
)
