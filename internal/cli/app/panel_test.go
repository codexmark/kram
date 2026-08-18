package app

import (
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/openai"
)

func TestFocusedRankingFindsMatchingProvider(t *testing.T) {
	m := Model{routeCall: &daemonclient.RouteCall{
		Ranking: []openai.RankedProviderInfo{
			{Provider: "a", Score: 0.9},
			{Provider: "b", Score: 0.5},
		},
	}}
	info, ok := m.focusedRanking("b")
	if !ok {
		t.Fatal("expected to find provider b in the ranking")
	}
	if info.Score != 0.5 {
		t.Errorf("score = %v, want 0.5", info.Score)
	}
}

func TestFocusedRankingReturnsFalseWhenNoRouteCall(t *testing.T) {
	m := Model{}
	if _, ok := m.focusedRanking("anything"); ok {
		t.Error("expected no ranking before any turn has run")
	}
}

func TestFocusedRankingReturnsFalseWhenProviderNotRanked(t *testing.T) {
	m := Model{routeCall: &daemonclient.RouteCall{
		Ranking: []openai.RankedProviderInfo{{Provider: "a", Score: 0.9}},
	}}
	if _, ok := m.focusedRanking("nonexistent"); ok {
		t.Error("expected false for a provider that never appeared in the ranking")
	}
}

// TestRenderScoreBreakdownIncludesAllFactors is the regression test for a
// real bug found live: the strategy panel's provider-list header plus a
// full score breakdown together exceeded the shared panel height, so
// padLines silently truncated the output before the factor lines ever
// rendered — even though the underlying data (verified directly against
// the gateway's raw SSE output) was always complete. This checks the
// breakdown-rendering function in isolation, so a future height/layout
// change can't quietly reintroduce the same class of bug undetected.
func TestRenderScoreBreakdownIncludesAllFactors(t *testing.T) {
	info := openai.RankedProviderInfo{
		Provider: "anthropic", Score: 0.910,
		Factors: []openai.ScoreFactor{
			{Name: "health", Weight: 0.30, Value: 1.0, Contribution: 0.300},
			{Name: "reliability", Weight: 0.20, Value: 0.96, Contribution: 0.192},
			{Name: "latency", Weight: 0.15, Value: 0.72, Contribution: 0.108},
			{Name: "quality", Weight: 0.15, Value: 0.90, Contribution: 0.135},
			{Name: "cache_affinity", Weight: 0.15, Value: 1.0, Contribution: 0.150},
			{Name: "priority", Weight: 0.05, Value: 0.50, Contribution: 0.025},
		},
		Reasons: []string{"last-known-good", "cache-affinity"},
	}
	lines := renderScoreBreakdown(info)
	joined := strings.Join(lines, "\n")

	for _, factorName := range []string{"health", "reliability", "latency", "quality", "cache_affinity", "priority"} {
		if !strings.Contains(joined, factorName) {
			t.Errorf("score breakdown is missing factor %q — output:\n%s", factorName, joined)
		}
	}
	if !strings.Contains(joined, "anthropic") {
		t.Error("score breakdown is missing the provider name")
	}
	if !strings.Contains(joined, "LAST-KNOWN-GOOD") && !strings.Contains(strings.ToUpper(joined), "LAST-KNOWN-GOOD") {
		t.Errorf("score breakdown is missing its reasons — output:\n%s", joined)
	}
}

func TestRenderScoreBreakdownHandlesNoFactorsOrReasons(t *testing.T) {
	// A strategy that scores but has no factor data yet (shouldn't
	// happen with the weighted engine, but must not panic) still
	// produces a valid, non-empty breakdown.
	info := openai.RankedProviderInfo{Provider: "x", Score: 0.5}
	lines := renderScoreBreakdown(info)
	if len(lines) == 0 {
		t.Error("expected at least the provider/score header line")
	}
}

func TestOtherScoresExcludesFocusedProvider(t *testing.T) {
	ranking := []openai.RankedProviderInfo{
		{Provider: "a", Score: 0.9},
		{Provider: "b", Score: 0.7},
		{Provider: "c", Score: 0.5},
	}
	lines := otherScores(ranking, "b")
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "b ") || strings.HasPrefix(joined, "b") {
		t.Errorf("otherScores should exclude the focused provider, got: %s", joined)
	}
	if !strings.Contains(joined, "a") || !strings.Contains(joined, "c") {
		t.Errorf("otherScores should include every other ranked candidate, got: %s", joined)
	}
}

func TestRenderRoutePanelEmptyWhenNoTrace(t *testing.T) {
	m := Model{width: 80, height: 40}
	got := m.renderRoutePanel()
	if strings.Contains(got, "#1") {
		t.Error("expected no call sections before any turn has completed")
	}
}

// TestRenderRoutePanelShowsMultipleCalls complements route_test.go's
// agent-layer regression test at the UI layer: a turn with several model
// calls must show all of them, not just the last.
func TestRenderRoutePanelShowsMultipleCalls(t *testing.T) {
	m := Model{
		width: 80, height: 40,
		routeTrace: daemonclient.RouteTrace{
			Strategy: "smart",
			Calls: []daemonclient.RouteCall{
				{Index: 1, Attempts: []openai.AttemptInfo{{Provider: "a", Outcome: openai.OutcomeSuccess, LatencyMS: 100}}},
				{Index: 2, Attempts: []openai.AttemptInfo{
					{Provider: "a", Outcome: openai.OutcomeError, LatencyMS: 50, Reason: "upstream 429"},
					{Provider: "b", Outcome: openai.OutcomeSuccess, LatencyMS: 300},
				}},
				{Index: 3, Attempts: []openai.AttemptInfo{{Provider: "b", Outcome: openai.OutcomeSuccess, LatencyMS: 200}}},
			},
		},
	}
	got := m.renderRoutePanel()
	for _, marker := range []string{"#1", "#2", "#3", "upstream 429"} {
		if !strings.Contains(got, marker) {
			t.Errorf("route panel is missing %q — every model call and its real failure reason should be visible, got:\n%s", marker, got)
		}
	}
	if !strings.Contains(got, "3") { // "3 chamadas de modelo" summary
		t.Error("expected the summary to report 3 model calls")
	}
}

func TestRouteAttemptDetailDistinguishesRejectedFromError(t *testing.T) {
	rejected := routeAttemptDetail(openai.AttemptInfo{Outcome: openai.OutcomeRejected, Reason: "empty response"}, openai.RankedProviderInfo{})
	errored := routeAttemptDetail(openai.AttemptInfo{Outcome: openai.OutcomeError, Reason: "upstream 500"}, openai.RankedProviderInfo{})
	if rejected != "empty response" {
		t.Errorf("rejected detail = %q, want the real reason text", rejected)
	}
	if errored != "upstream 500" {
		t.Errorf("errored detail = %q, want the real reason text", errored)
	}
}
