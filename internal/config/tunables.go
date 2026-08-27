package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Default tunables — the values that were compiled-in constants before this
// block existed, so an absent `tunables:` section reproduces exactly the old
// behavior. See DECISIONS.md ("Configurable timeouts and breaker tunables").
const (
	defaultProviderTimeout       = 120 * time.Second
	defaultGatewayClientFloor    = 180 * time.Second
	defaultBreakerFailureThresh  = 3
	defaultBreakerCooldown       = 30 * time.Second
	gatewayClientCoherenceMargin = 30 * time.Second // headroom over N×provider so a fallback round is never cut mid-flight
)

// Duration is a time.Duration that marshals to / from a Go duration string
// ("120s", "2m", "1m30s") in YAML. yaml.v3 would otherwise decode a bare
// number as nanoseconds, which is a footgun for a human-edited config file —
// "120" meaning 120ns is never what someone means by a timeout. The zero
// value marshals to nothing (omitempty-friendly) and is read as "unset, use
// the default" everywhere it's consumed.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string; an empty string is 0 (unset).
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"120s\": %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML writes the duration as a string, or nothing when it's the
// zero value, so a Save round-trip never litters the file with "0s" lines
// for tunables the user never set.
func (d Duration) MarshalYAML() (any, error) {
	if d == 0 {
		return nil, nil
	}
	return time.Duration(d).String(), nil
}

// Tunables holds operational timeouts and circuit-breaker thresholds that
// used to be compiled-in constants. Every field is optional; a zero value
// means "use the built-in default", so an existing config.yaml with no
// tunables block behaves exactly as before. This exists because Kram
// targets local models, where latency varies from seconds to minutes with
// the model and cold-load — fixed timeouts calibrated for fast hosted APIs
// cut off healthy-but-slow responses and force fallback to worse
// candidates. See DECISIONS.md.
type Tunables struct {
	// ProviderTimeout caps a single upstream request (dial + headers +
	// reading the whole body) per provider. Default 120s. Generous values
	// are the point for slow local models; a genuinely dead provider is
	// still cut eventually.
	ProviderTimeout Duration `yaml:"provider_timeout,omitempty"`
	// GatewayClientTimeout caps the daemon's whole call to the gateway,
	// which may itself walk a fallback chain of several providers. Left
	// unset it's *derived* to stay coherent with the chain (see
	// ResolvedGatewayClientTimeout) rather than defaulting to a fixed value
	// that a legitimate multi-candidate round could exceed.
	GatewayClientTimeout Duration `yaml:"gateway_client_timeout,omitempty"`
	// BreakerFailureThreshold is how many consecutive failures trip a
	// provider's circuit breaker open. Default 3.
	BreakerFailureThreshold int `yaml:"breaker_failure_threshold,omitempty"`
	// BreakerCooldown is how long a tripped provider stays open before a
	// half-open trial request. Default 30s.
	BreakerCooldown Duration `yaml:"breaker_cooldown,omitempty"`
}

// ResolvedProviderTimeout is the per-provider request timeout to use,
// substituting the default when unset.
func (t Tunables) ResolvedProviderTimeout() time.Duration {
	if t.ProviderTimeout > 0 {
		return t.ProviderTimeout.Duration()
	}
	return defaultProviderTimeout
}

// ResolvedBreakerFailureThreshold is the breaker trip threshold, defaulted.
func (t Tunables) ResolvedBreakerFailureThreshold() int {
	if t.BreakerFailureThreshold > 0 {
		return t.BreakerFailureThreshold
	}
	return defaultBreakerFailureThresh
}

// ResolvedBreakerCooldown is the breaker cooldown, defaulted.
func (t Tunables) ResolvedBreakerCooldown() time.Duration {
	if t.BreakerCooldown > 0 {
		return t.BreakerCooldown.Duration()
	}
	return defaultBreakerCooldown
}

// ResolvedGatewayClientTimeout returns the timeout the daemon's gateway
// client should use, given the longest fallback chain it might drive.
//
// If the user pinned a value, that's honored verbatim (their call). Left
// unset, it's *derived* so the client never cuts off a legitimate fallback
// round: the gateway may try every provider in the longest combo back to
// back, so the client must allow at least maxComboLen × providerTimeout,
// plus a small margin. This is the incoherence the audit flagged — a fixed
// 180s client timeout is smaller than 2×120s, so a healthy two-candidate
// fallback could be killed by the client before the chain was exhausted.
// The result is floored at defaultGatewayClientFloor so single-provider
// setups keep today's generous ceiling.
func (t Tunables) ResolvedGatewayClientTimeout(maxComboLen int) time.Duration {
	if t.GatewayClientTimeout > 0 {
		return t.GatewayClientTimeout.Duration()
	}
	if maxComboLen < 1 {
		maxComboLen = 1
	}
	derived := time.Duration(maxComboLen)*t.ResolvedProviderTimeout() + gatewayClientCoherenceMargin
	if derived < defaultGatewayClientFloor {
		return defaultGatewayClientFloor
	}
	return derived
}

// MaxComboLength returns the provider count of the longest combo — the
// worst-case number of upstreams a single gateway call might try before
// giving up, which sets the coherent lower bound for the gateway-client
// timeout.
func (c *Config) MaxComboLength() int {
	max := 0
	for _, combo := range c.Combos {
		if n := len(combo.Providers); n > max {
			max = n
		}
	}
	return max
}
