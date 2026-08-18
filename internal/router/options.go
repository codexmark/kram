package router

import "github.com/codexmark/kram-gateway/internal/config"

// Defaults applied when a combo's strategy_options omits a field — chosen
// to make "strategy: smart" alone (no strategy_options block at all)
// behave well: sticky by default (the whole point of Smart Sticky, see
// DECISIONS.md), a modest LKGP boost, and a conservative exploration rate
// that won't noticeably affect routing quality but still lets non-winning
// candidates accumulate telemetry over time.
const (
	defaultSticky      = true
	defaultLKGPBoost   = 0.10
	defaultExploration = 0.03
)

// strategyOptions is the resolved, defaulted form of
// config.StrategyOptions — config's pointer fields (nil meaning "use the
// default") are resolved exactly once, at combo-build time, so every
// Strategy implementation works with plain concrete values instead of
// re-deriving defaults on every request.
type strategyOptions struct {
	sticky      bool
	lkgpBoost   float64
	exploration float64
	weights     map[string]float64
}

func resolveStrategyOptions(cc config.StrategyOptions) strategyOptions {
	opts := strategyOptions{
		sticky: defaultSticky, lkgpBoost: defaultLKGPBoost, exploration: defaultExploration,
		weights: cc.Weights,
	}
	if cc.Sticky != nil {
		opts.sticky = *cc.Sticky
	}
	if cc.LKGPBoost != nil {
		opts.lkgpBoost = *cc.LKGPBoost
	}
	if cc.Exploration != nil {
		opts.exploration = *cc.Exploration
	}
	return opts
}
