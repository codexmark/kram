package router

import (
	"context"
	"testing"

	"github.com/codexmark/kram-gateway/internal/breaker"
	"github.com/codexmark/kram-gateway/internal/config"
	"github.com/codexmark/kram-gateway/internal/openai"
	"github.com/codexmark/kram-gateway/internal/provider"
	"github.com/codexmark/kram-gateway/internal/telemetry"
)

// fakeProvider is the minimum needed to satisfy provider.Provider for
// routing tests — no real HTTP involved, since Rank only cares about
// ID/ordering/capabilities, never actually calling ChatCompletion.
type fakeProvider struct {
	id     string
	tools  bool
	images bool
}

func (f fakeProvider) ID() string           { return f.id }
func (f fakeProvider) Kind() string         { return "fake" }
func (f fakeProvider) SupportsImages() bool { return f.images }
func (f fakeProvider) SupportsTools() bool  { return f.tools }
func (f fakeProvider) ChatCompletion(context.Context, openai.ChatCompletionRequest) (<-chan provider.StreamEvent, error) {
	panic("not used by router tests")
}

func newTestRouter(t *testing.T, strategy string, ids ...string) (*Router, *breaker.Registry) {
	t.Helper()
	providers := make(map[string]provider.Provider, len(ids))
	for _, id := range ids {
		providers[id] = fakeProvider{id: id, tools: true, images: true}
	}
	cfg := &config.Config{
		Combos:       []config.ComboConfig{{ID: "default", Strategy: strategy, Providers: ids}},
		DefaultCombo: "default",
	}
	breakers := breaker.NewRegistry()
	r, err := New(cfg, providers, breakers, telemetry.New())
	if err != nil {
		t.Fatal(err)
	}
	return r, breakers
}

// reqWithAffinity builds a minimal request whose AffinityKey is exactly
// key — a single user message and no system messages, so AffinityKey's
// "system + first user message" prefix collapses to just that content.
func reqWithAffinity(key string) openai.ChatCompletionRequest {
	return openai.ChatCompletionRequest{Messages: []openai.ChatMessage{{Role: "user", Content: key}}}
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
	r, err := New(cfg, map[string]provider.Provider{"a": fakeProvider{id: "a", tools: true}}, breaker.NewRegistry(), telemetry.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("y"); err == nil {
		t.Error("expected an error when nothing matches and no default_combo is set")
	}
}

func TestNewRejectsUnknownStrategy(t *testing.T) {
	cfg := &config.Config{Combos: []config.ComboConfig{{ID: "x", Strategy: "made-up-strategy", Providers: []string{"a"}}}}
	if _, err := New(cfg, map[string]provider.Provider{"a": fakeProvider{id: "a", tools: true}}, breaker.NewRegistry(), telemetry.New()); err == nil {
		t.Error("expected New to reject an unrecognized strategy name")
	}
}

func TestDeclaredOrderStrategyNeverRotates(t *testing.T) {
	// Empty strategy string — the "paid provider leads" case from
	// autoStrategy — must preserve exact declared order every call.
	r, _ := newTestRouter(t, "", "a", "b", "c")
	for i := 0; i < 5; i++ {
		ranked, _, err := r.Rank("default", reqWithAffinity("same-prefix"))
		if err != nil {
			t.Fatal(err)
		}
		if got := rankedIDs(ranked); got != "a,b,c" {
			t.Fatalf("call %d: got order %s, want a,b,c every time", i, got)
		}
	}
}

func TestPriorityAliasSameAsEmpty(t *testing.T) {
	r, _ := newTestRouter(t, "priority", "a", "b", "c")
	ranked, _, err := r.Rank("default", reqWithAffinity("x"))
	if err != nil {
		t.Fatal(err)
	}
	if got := rankedIDs(ranked); got != "a,b,c" {
		t.Fatalf("priority strategy: got %s, want a,b,c", got)
	}
}

func TestRoundRobinRotates(t *testing.T) {
	r, _ := newTestRouter(t, "round-robin", "a", "b", "c")
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		ranked, _, err := r.Rank("default", reqWithAffinity(""))
		if err != nil {
			t.Fatal(err)
		}
		seen[rankedIDs(ranked)] = true
	}
	if len(seen) < 2 {
		t.Errorf("round-robin should rotate the leading provider across calls, only saw: %v", seen)
	}
}

func TestPrefixAffinityIsStableForTheSameKey(t *testing.T) {
	r, _ := newTestRouter(t, "prefix-affinity", "a", "b", "c")
	first, _, err := r.Rank("default", reqWithAffinity("identical prompt prefix"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, _, err := r.Rank("default", reqWithAffinity("identical prompt prefix"))
		if err != nil {
			t.Fatal(err)
		}
		if rankedIDs(again) != rankedIDs(first) {
			t.Fatalf("prefix-affinity should return the same order for the same key: got %s then %s", rankedIDs(first), rankedIDs(again))
		}
	}
}

func TestPrefixAffinityDiffersAcrossKeys(t *testing.T) {
	r, _ := newTestRouter(t, "prefix-affinity", "a", "b", "c", "d", "e")
	leaders := map[string]bool{}
	for i := 0; i < 20; i++ {
		ranked, _, err := r.Rank("default", reqWithAffinity(string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		leaders[ranked[0].Provider.Provider.ID()] = true
	}
	if len(leaders) < 2 {
		t.Errorf("different affinity keys should generally pick different leaders across 20 tries, only saw: %v", leaders)
	}
}

func TestRankSkipsOpenBreaker(t *testing.T) {
	r, breakers := newTestRouter(t, "round-robin", "a", "b")
	breakers.ReportFailure("a")
	breakers.ReportFailure("a")
	breakers.ReportFailure("a") // trips open at the default threshold

	ranked, _, err := r.Rank("default", reqWithAffinity(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, rc := range ranked {
		if rc.Provider.Provider.ID() == "a" {
			t.Error("a tripped-open provider should never appear in Rank's output")
		}
	}
}

func TestRankErrorsWhenAllBreakersOpen(t *testing.T) {
	r, breakers := newTestRouter(t, "round-robin", "a")
	for i := 0; i < 3; i++ {
		breakers.ReportFailure("a")
	}
	if _, _, err := r.Rank("default", reqWithAffinity("")); err == nil {
		t.Error("expected an error when every provider in the combo is circuit-open")
	}
}

func TestRankUnknownCombo(t *testing.T) {
	r, _ := newTestRouter(t, "round-robin", "a")
	if _, _, err := r.Rank("does-not-exist", reqWithAffinity("")); err == nil {
		t.Error("expected an error for an unknown combo id")
	}
}

func TestRankExcludesProvidersMissingRequiredCapability(t *testing.T) {
	providers := map[string]provider.Provider{
		"has-tools":  fakeProvider{id: "has-tools", tools: true},
		"no-tools":   fakeProvider{id: "no-tools", tools: false},
		"has-images": fakeProvider{id: "has-images", images: true},
		"no-images":  fakeProvider{id: "no-images"},
	}
	cfg := &config.Config{
		Combos: []config.ComboConfig{{
			ID: "default", Strategy: "priority",
			Providers: []string{"has-tools", "no-tools", "has-images", "no-images"},
		}},
		DefaultCombo: "default",
	}
	r, err := New(cfg, providers, breaker.NewRegistry(), telemetry.New())
	if err != nil {
		t.Fatal(err)
	}

	req := reqWithAffinity("x")
	req.Tools = []openai.Tool{{Type: "function", Function: openai.ToolFunction{Name: "f"}}}
	ranked, ctx, err := r.Rank("default", req)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.NeedsTools {
		t.Fatal("RouteContext should report NeedsTools for a request carrying tool definitions")
	}
	for _, rc := range ranked {
		if !rc.Provider.Provider.SupportsTools() {
			t.Errorf("a provider without tool support must never appear in ranking for a tool-using request, got %s", rc.Provider.Provider.ID())
		}
	}

	imgReq := reqWithAffinity("y")
	imgReq.Messages[0].Images = []string{"data:image/png;base64,xx"}
	ranked, ctx, err = r.Rank("default", imgReq)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.NeedsImages {
		t.Fatal("RouteContext should report NeedsImages for a request carrying image content")
	}
	for _, rc := range ranked {
		if !rc.Provider.Provider.SupportsImages() {
			t.Errorf("a provider without image support must never appear in ranking for an image-carrying request, got %s", rc.Provider.Provider.ID())
		}
	}
}

func rankedIDs(ranked []RankedCandidate) string {
	out := ""
	for i, rc := range ranked {
		if i > 0 {
			out += ","
		}
		out += rc.Provider.Provider.ID()
	}
	return out
}
