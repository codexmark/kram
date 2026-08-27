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
	strategyPickerSaving         = "saving as default…"
	strategyPickerHint           = "↑↓ choose · enter apply · ctrl+s save default · esc back"
	strategyPickerNoComboErr     = "gateway hasn't reported combo and strategies yet"
)

// Combo picker chrome (the first level of the Ctrl+S routing panel — pick
// which combo future messages route through before choosing its strategy).
const (
	comboPickerTitle          = "route · select combo"
	comboPickerLoading        = "loading combos from the gateway…"
	comboPickerActiveBadge    = "ACTIVE"
	comboPickerHint           = "↑↓ choose · enter select · esc close"
	comboPickerSingleProvider = "single provider — no routing to configure"
	comboProvidersSingular    = "provider"
	comboProvidersPlural      = "providers"
	strategySavedPrefix       = "✓ saved default: "
	comboSwitchedPrefix       = "✓ combo: "
	comboSingleNoticeSuffix   = " (single provider — no routing)"
)
