package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDurationUnmarshalYAML(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: `"120s"`, want: 120 * time.Second},
		{in: `"2m"`, want: 2 * time.Minute},
		{in: `"1m30s"`, want: 90 * time.Second},
		{in: `""`, want: 0},
		{in: `"nonsense"`, wantErr: true},
		{in: `"120"`, wantErr: true}, // bare number without a unit is not a valid Go duration
	}
	for _, tc := range cases {
		var d Duration
		err := yaml.Unmarshal([]byte(tc.in), &d)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Unmarshal(%s): expected error, got %v", tc.in, d.Duration())
			}
			continue
		}
		if err != nil {
			t.Errorf("Unmarshal(%s): unexpected error: %v", tc.in, err)
			continue
		}
		if d.Duration() != tc.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", tc.in, d.Duration(), tc.want)
		}
	}
}

func TestDurationMarshalYAMLOmitsZero(t *testing.T) {
	// A zero Duration inside a struct with omitempty must not emit a line —
	// otherwise a Save round-trip litters the config with "0s" for every
	// tunable the user never set.
	type holder struct {
		D Duration `yaml:"d,omitempty"`
	}
	out, err := yaml.Marshal(holder{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "{}\n" {
		t.Errorf("zero Duration marshaled to %q, want an empty struct", got)
	}

	out, err = yaml.Marshal(holder{D: Duration(90 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "d: 1m30s\n" {
		t.Errorf("set Duration marshaled to %q, want %q", got, "d: 1m30s\n")
	}
}

func TestTunablesResolveDefaults(t *testing.T) {
	var z Tunables
	if got := z.ResolvedProviderTimeout(); got != defaultProviderTimeout {
		t.Errorf("ResolvedProviderTimeout default = %v, want %v", got, defaultProviderTimeout)
	}
	if got := z.ResolvedBreakerFailureThreshold(); got != defaultBreakerFailureThresh {
		t.Errorf("ResolvedBreakerFailureThreshold default = %d, want %d", got, defaultBreakerFailureThresh)
	}
	if got := z.ResolvedBreakerCooldown(); got != defaultBreakerCooldown {
		t.Errorf("ResolvedBreakerCooldown default = %v, want %v", got, defaultBreakerCooldown)
	}
}

func TestTunablesResolveOverrides(t *testing.T) {
	tn := Tunables{
		ProviderTimeout:         Duration(300 * time.Second),
		BreakerFailureThreshold: 5,
		BreakerCooldown:         Duration(10 * time.Second),
	}
	if got := tn.ResolvedProviderTimeout(); got != 300*time.Second {
		t.Errorf("ResolvedProviderTimeout = %v, want 300s", got)
	}
	if got := tn.ResolvedBreakerFailureThreshold(); got != 5 {
		t.Errorf("ResolvedBreakerFailureThreshold = %d, want 5", got)
	}
	if got := tn.ResolvedBreakerCooldown(); got != 10*time.Second {
		t.Errorf("ResolvedBreakerCooldown = %v, want 10s", got)
	}
}

// TestResolvedGatewayClientTimeoutCoherence is the core of the audit fix:
// with defaults, the derived client timeout must be at least
// maxComboLen × providerTimeout, so a legitimate fallback round walking
// every provider in the longest combo is never cut off client-side.
func TestResolvedGatewayClientTimeoutCoherence(t *testing.T) {
	var z Tunables // defaults: provider timeout 120s, floor 180s

	// Single provider: the floor dominates (1×120+30 = 150 < 180).
	if got := z.ResolvedGatewayClientTimeout(1); got != defaultGatewayClientFloor {
		t.Errorf("maxComboLen=1: got %v, want floor %v", got, defaultGatewayClientFloor)
	}
	// maxComboLen=0 is treated as 1 (never derive a sub-floor value).
	if got := z.ResolvedGatewayClientTimeout(0); got != defaultGatewayClientFloor {
		t.Errorf("maxComboLen=0: got %v, want floor %v", got, defaultGatewayClientFloor)
	}
	// Three providers: 3×120 + 30 margin = 390s, which exceeds the floor.
	want := 3*defaultProviderTimeout + gatewayClientCoherenceMargin
	if got := z.ResolvedGatewayClientTimeout(3); got != want {
		t.Errorf("maxComboLen=3: got %v, want %v", got, want)
	}
	if got := z.ResolvedGatewayClientTimeout(3); got <= 3*defaultProviderTimeout {
		t.Errorf("derived timeout %v must exceed 3×provider timeout %v", got, 3*defaultProviderTimeout)
	}
}

func TestResolvedGatewayClientTimeoutHonorsPinnedValue(t *testing.T) {
	tn := Tunables{GatewayClientTimeout: Duration(45 * time.Second)}
	// A pinned value is honored verbatim even if it's below what coherence
	// would derive — the user asked for it explicitly.
	if got := tn.ResolvedGatewayClientTimeout(10); got != 45*time.Second {
		t.Errorf("pinned gateway_client_timeout = %v, want 45s", got)
	}
}

func TestMaxComboLength(t *testing.T) {
	c := &Config{Combos: []ComboConfig{
		{ID: "a", Providers: []string{"p1"}},
		{ID: "b", Providers: []string{"p1", "p2", "p3"}},
		{ID: "c", Providers: []string{"p1", "p2"}},
	}}
	if got := c.MaxComboLength(); got != 3 {
		t.Errorf("MaxComboLength = %d, want 3", got)
	}
	if got := (&Config{}).MaxComboLength(); got != 0 {
		t.Errorf("MaxComboLength of empty config = %d, want 0", got)
	}
}

func TestValidateRejectsNegativeTunables(t *testing.T) {
	base := func() Config {
		return Config{
			Providers: []ProviderConfig{{ID: "p", Kind: "anthropic", APIKeyEnv: "K"}},
			Combos:    []ComboConfig{{ID: "c", Providers: []string{"p"}}},
		}
	}
	cases := map[string]func(*Config){
		"provider_timeout":          func(c *Config) { c.Tunables.ProviderTimeout = -1 },
		"gateway_client_timeout":    func(c *Config) { c.Tunables.GatewayClientTimeout = -1 },
		"breaker_cooldown":          func(c *Config) { c.Tunables.BreakerCooldown = -1 },
		"breaker_failure_threshold": func(c *Config) { c.Tunables.BreakerFailureThreshold = -1 },
	}
	for name, mutate := range cases {
		c := base()
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("negative %s: expected validate() to error", name)
		}
	}
	// A config with no tunables at all still validates.
	c := base()
	if err := c.validate(); err != nil {
		t.Errorf("config with no tunables should validate, got %v", err)
	}
}
