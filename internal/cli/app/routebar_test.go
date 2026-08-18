package app

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
	"github.com/codexmark/kram-gateway/internal/openai"
)

func TestRenderRouteBarEmptyWhenNoStrategyKnown(t *testing.T) {
	m := Model{width: 80}
	if got := m.renderRouteBar(); got != "" {
		t.Errorf("expected an empty route bar before status has ever loaded, got %q", got)
	}
}

func TestRenderRouteBarShowsStrategyFromCombo(t *testing.T) {
	m := Model{
		width: 80, combo: "default",
		strategyData: statusclient.Status{Combos: []statusclient.Combo{{ID: "default", Strategy: "smart"}}},
	}
	got := m.renderRouteBar()
	if !strings.Contains(got, "SMART") {
		t.Errorf("expected the route bar to show the combo's strategy name, got %q", got)
	}
}

func TestRenderRouteBarEmptyStrategyMeansPriority(t *testing.T) {
	m := Model{
		width: 80, combo: "default",
		strategyData: statusclient.Status{Combos: []statusclient.Combo{{ID: "default", Strategy: ""}}},
	}
	got := m.renderRouteBar()
	if !strings.Contains(strings.ToUpper(got), "PRIORITY") {
		t.Errorf("an empty strategy string should render as priority (v0's declared-order default), got %q", got)
	}
}

func TestRenderRouteBarShowsRunningPulse(t *testing.T) {
	m := Model{
		width: 80, combo: "default", routeRunning: true,
		strategyData: statusclient.Status{Combos: []statusclient.Combo{{ID: "default", Strategy: "smart"}}},
	}
	got := m.renderRouteBar()
	if !strings.Contains(got, "roteando") {
		t.Errorf("expected a running pulse while routeRunning is true, got %q", got)
	}
}

func TestRenderRouteBarShowsRealAttemptTrail(t *testing.T) {
	m := Model{
		width: 100, combo: "default",
		strategyData: statusclient.Status{Combos: []statusclient.Combo{{ID: "default", Strategy: "smart"}}},
		routeCall: &daemonclient.RouteCall{
			Index: 1, Strategy: "smart",
			Attempts: []openai.AttemptInfo{
				{Provider: "a", Outcome: openai.OutcomeError, LatencyMS: 100},
				{Provider: "b", Outcome: openai.OutcomeSuccess, LatencyMS: 200},
			},
		},
	}
	got := m.renderRouteBar()
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Errorf("expected both providers named in the wide-tier trail, got %q", got)
	}
	if !strings.Contains(got, "✕") || !strings.Contains(got, "✓") {
		t.Errorf("expected both outcome glyphs present, got %q", got)
	}
}

// TestRenderRouteBarNeverExceedsWidth is the regression test for a real
// bug found live: reflow's truncate unconditionally reserves room for its
// tail, so a line landing exactly at the terminal width got spuriously
// truncated with a trailing ellipsis and a clipped last character. This
// checks the bar's rendered width against a wide range of terminal
// widths and provider-name lengths, including long real-world provider
// IDs (openrouter-nemotron-3-super-120b is a real example from the
// catalog's naming style).
func TestRenderRouteBarNeverExceedsWidth(t *testing.T) {
	longAttempts := []openai.AttemptInfo{
		{Provider: "openrouter-nemotron-super-120b", Outcome: openai.OutcomeError, LatencyMS: 152},
		{Provider: "openrouter-gemma-4-31b-it-free", Outcome: openai.OutcomeError, LatencyMS: 641},
		{Provider: "anthropic-claude-sonnet-4-5", Outcome: openai.OutcomeSuccess, LatencyMS: 1420},
	}
	for width := 10; width <= 140; width++ {
		m := Model{
			width: width, combo: "default",
			strategyData: statusclient.Status{Combos: []statusclient.Combo{{ID: "default", Strategy: "smart"}}},
			routeCall:    &daemonclient.RouteCall{Index: 3, Strategy: "smart", Attempts: longAttempts},
		}
		got := m.renderRouteBar()
		if w := lipgloss.Width(got); w > width {
			t.Fatalf("width=%d: rendered route bar is %d cells wide, exceeding the terminal — %q", width, w, got)
		}
	}
}

func TestRenderRouteBarExactWidthLineNotSpuriouslyTruncated(t *testing.T) {
	// A line that lands exactly at the terminal width must render intact
	// — no dropped trailing character, no unnecessary ellipsis appended.
	m := Model{
		width: 60, combo: "default",
		strategyData: statusclient.Status{Combos: []statusclient.Combo{{ID: "default", Strategy: "smart"}}},
	}
	got := m.renderRouteBar()
	if strings.Contains(got, "…") {
		t.Errorf("a route bar with room to spare should never show a truncation ellipsis, got %q", got)
	}
}

func TestOutcomeGlyphDistinctPerOutcome(t *testing.T) {
	seen := map[string]bool{}
	for _, o := range []openai.AttemptOutcome{openai.OutcomeSuccess, openai.OutcomeError, openai.OutcomeRejected, openai.OutcomeSkipped, openai.OutcomeTrying} {
		g := outcomeGlyph(o)
		if g == "" {
			t.Errorf("outcome %q produced an empty glyph", o)
		}
		seen[g] = true
	}
	if len(seen) < 4 {
		t.Errorf("expected mostly-distinct glyphs across outcomes (never relying on color alone), got only %d distinct glyphs", len(seen))
	}
}

func TestFormatLatencySwitchesToSecondsAtOneThousandMS(t *testing.T) {
	if got := formatLatency(999); got != "999ms" {
		t.Errorf("formatLatency(999) = %q, want 999ms", got)
	}
	if got := formatLatency(1420); got != "1.4s" {
		t.Errorf("formatLatency(1420) = %q, want 1.4s", got)
	}
}
