package router

import (
	"context"
	"testing"

	"github.com/codexmark/kram-gateway/internal/breaker"
	"github.com/codexmark/kram-gateway/internal/config"
	"github.com/codexmark/kram-gateway/internal/openai"
	"github.com/codexmark/kram-gateway/internal/provider"
)

// fakeProvider is the minimum needed to satisfy provider.Provider for
// routing tests — no real HTTP involved, since Attempts only cares about
// ID/ordering, never actually calling ChatCompletion.
type fakeProvider struct{ id string }

func (f fakeProvider) ID() string           { return f.id }
func (f fakeProvider) Kind() string         { return "fake" }
func (f fakeProvider) SupportsImages() bool { return false }
func (f fakeProvider) SupportsTools() bool  { return false }
func (f fakeProvider) ChatCompletion(context.Context, openai.ChatCompletionRequest) (<-chan provider.StreamEvent, error) {
	panic("not used by router tests")
}

func newTestRouter(t *testing.T, strategy string, ids ...string) (*Router, *breaker.Registry) {
	t.Helper()
	providers := make(map[string]provider.Provider, len(ids))
	for _, id := range ids {
		providers[id] = fakeProvider{id: id}
	}
	cfg := &config.Config{
		Combos:       []config.ComboConfig{{ID: "default", Strategy: strategy, Providers: ids}},
		DefaultCombo: "default",
	}
	breakers := breaker.NewRegistry()
	r, err := New(cfg, providers, breakers)
	if err != nil {
		t.Fatal(err)
	}
	return r, breakers
}

func TestResolveExactComboWins(t *testing.T) {
	r, _ := newTestRouter(t, "round-robin", "a", "b")
	id, err := r.Resolve("default")
	if err != nil || id != "default" {
		t.Fatalf("Resolve(%q) = %q, %v", "default", id, err)
	}
}

func TestResolveFallsBackToDefault(t *testing.T) {
	r, _ := newTestRouter(t, "round-robin", "a")
	id, err := r.Resolve("nonexistent-model")
	if err != nil || id != "default" {
		t.Fatalf("Resolve should fall back to default combo, got %q, %v", id, err)
	}
}

func TestResolveErrorsWithNoMatchAndNoDefault(t *testing.T) {
	cfg := &config.Config{Combos: []config.ComboConfig{{ID: "x", Strategy: "round-robin", Providers: []string{"a"}}}}
	r, err := New(cfg, map[string]provider.Provider{"a": fakeProvider{id: "a"}}, breaker.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("y"); err == nil {
		t.Error("expected an error when nothing matches and no default_combo is set")
	}
}

func TestDeclaredOrderStrategyNeverRotates(t *testing.T) {
	// Empty strategy string — the "paid provider leads" case from
	// autoStrategy — must preserve exact declared order every call.
	r, _ := newTestRouter(t, "", "a", "b", "c")
	for i := 0; i < 5; i++ {
		attempts, err := r.Attempts("default", "same-prefix")
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(attempts); got != "a,b,c" {
			t.Fatalf("call %d: got order %s, want a,b,c every time", i, got)
		}
	}
}

func TestRoundRobinRotates(t *testing.T) {
	r, _ := newTestRouter(t, "round-robin", "a", "b", "c")
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		attempts, err := r.Attempts("default", "")
		if err != nil {
			t.Fatal(err)
		}
		seen[ids(attempts)] = true
	}
	if len(seen) < 2 {
		t.Errorf("round-robin should rotate the leading provider across calls, only saw: %v", seen)
	}
}

func TestPrefixAffinityIsStableForTheSameKey(t *testing.T) {
	r, _ := newTestRouter(t, StrategyPrefixAffinity, "a", "b", "c")
	first, err := r.Attempts("default", "identical prompt prefix")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := r.Attempts("default", "identical prompt prefix")
		if err != nil {
			t.Fatal(err)
		}
		if ids(again) != ids(first) {
			t.Fatalf("prefix-affinity should return the same order for the same key: got %s then %s", ids(first), ids(again))
		}
	}
}

func TestPrefixAffinityDiffersAcrossKeys(t *testing.T) {
	r, _ := newTestRouter(t, StrategyPrefixAffinity, "a", "b", "c", "d", "e")
	leaders := map[string]bool{}
	for i := 0; i < 20; i++ {
		attempts, err := r.Attempts("default", string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		leaders[attempts[0].ID()] = true
	}
	if len(leaders) < 2 {
		t.Errorf("different affinity keys should generally pick different leaders across 20 tries, only saw: %v", leaders)
	}
}

func TestAttemptsSkipsOpenBreaker(t *testing.T) {
	r, breakers := newTestRouter(t, "round-robin", "a", "b")
	breakers.ReportFailure("a")
	breakers.ReportFailure("a")
	breakers.ReportFailure("a") // trips open at the default threshold

	attempts, err := r.Attempts("default", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range attempts {
		if p.ID() == "a" {
			t.Error("a tripped-open provider should never appear in Attempts")
		}
	}
}

func TestAttemptsErrorsWhenAllBreakersOpen(t *testing.T) {
	r, breakers := newTestRouter(t, "round-robin", "a")
	for i := 0; i < 3; i++ {
		breakers.ReportFailure("a")
	}
	if _, err := r.Attempts("default", ""); err == nil {
		t.Error("expected an error when every provider in the combo is circuit-open")
	}
}

func TestAttemptsUnknownCombo(t *testing.T) {
	r, _ := newTestRouter(t, "round-robin", "a")
	if _, err := r.Attempts("does-not-exist", ""); err == nil {
		t.Error("expected an error for an unknown combo id")
	}
}

func ids(ps []provider.Provider) string {
	out := ""
	for i, p := range ps {
		if i > 0 {
			out += ","
		}
		out += p.ID()
	}
	return out
}
