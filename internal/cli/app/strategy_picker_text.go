package app

// strategyDesc* are the one-line explanations shown under the selected
// strategy in the strategy picker. Keyed into strategyDescriptions by the
// gateway's internal strategy id.
const (
	strategyDescPriority       = "declared order; uses the first healthy provider"
	strategyDescRoundRobin     = "rotates the first provider between calls"
	strategyDescPrefixAffinity = "keeps similar prefixes on the same provider"
	strategyDescSmart          = "balances health, quality, latency and affinity"
	strategyDescQuality        = "prioritizes the quality estimate"
	strategyDescFast           = "prioritizes lowest observed latency"
	strategyDescCheap          = "prioritizes lowest configured cost"
	strategyDescReliable       = "prioritizes success history"
	strategyDescWeighted       = "uses the combo's custom weights"
	strategyDescLKGP           = "prefers the last provider that responded well"
	strategyDescP2C            = "compares two candidates and picks the healthier one"
	strategyDescFallback       = "strategy provided by the gateway"
)

// Strategy picker chrome: title, loading and error messages, the active
// badge, the in-flight/failed states and the footer hint.
const (
	strategyPickerTitlePrefix    = "switch strategy · combo "
	strategyPickerLoading        = "loading strategies from the gateway…"
	strategyPickerQueryErrPrefix = "couldn't query the gateway: "
	strategyPickerActiveBadge    = "ACTIVE"
	strategyPickerFailPrefix     = "failed: "
	strategyPickerApplying       = "applying on the next call…"
	strategyPickerHint           = "↑↓ choose · enter apply · esc cancel · click applies"
	strategyPickerNoComboErr     = "gateway hasn't reported combo and strategies yet"
)
