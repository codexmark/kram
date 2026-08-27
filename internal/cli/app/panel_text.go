package app

// Strategy panel strings (status area): the provider fallback chain,
// circuit-breaker badges, and the focused-provider explanation, all rendered
// from live gateway telemetry.
const (
	strategyGatewayErrPrefix = "couldn't reach the gateway: "
	strategyNoCombo          = "no combo configured in the gateway"
	strategyComboLine        = "combo %s · strategy %s"
	strategySwitchCandidate  = "↑↓ switch candidate"

	providerNoData   = "no data yet"
	providerUnstable = "unstable"
	providerSuccess  = "%.0f%% success"

	explainEntersRotation = "enters the rotation"
	explainFirstFallback  = "first in the fallback order"
	explainTakesOver      = "takes over if the %d before it fail or have an open circuit"
	explainCircuitOpen    = "▸ %s: circuit open now — being skipped until the next automatic probe. %s."
	explainNoRequests     = "▸ %s: %s. No requests yet in this gateway session."
	explainStats          = "▸ %s: %s. %d requests, %.0f%% success, %dms average latency."
)
