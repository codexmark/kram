package app

import (
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/cli/statusclient"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/providercatalog"
	"github.com/codexmark/kram/internal/providerping"
)

// This file covers the package's pure, easily-isolable helper functions
// — formatting, lookups, and simple decision logic that don't need a
// running Bubble Tea program to exercise. The bulk of internal/cli/app
// is View()/Update() rendering and tea.Cmd closures that do real I/O
// (daemon/gateway calls, OAuth flows, terminal rendering) — those are
// deliberately left uncovered here: a unit test that just checks a
// lipgloss-rendered string is "non-empty" verifies almost nothing, and
// the actual golden-path/edge-case coverage for this package comes from
// exercising it in a real terminal (see this session's browser/tmux
// verification discipline), not from asserting on ANSI byte soup.

func TestFormatAgeBuckets(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "now"},
		{5 * time.Minute, "5min ago"},
		{3 * time.Hour, "3h ago"},
		{2 * 24 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		got := formatAge(now.Add(-tc.ago).Unix())
		if got != tc.want {
			t.Errorf("formatAge(now-%v) = %q, want %q", tc.ago, got, tc.want)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{462900, "462.9k"},
	}
	for _, tc := range cases {
		if got := formatTokens(tc.n); got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestContextBarDrawsProportionalSegments(t *testing.T) {
	u := daemonclient.ContextUsage{
		Budget: 100,
		Categories: []daemonclient.ContextCategory{
			{Name: "system", Tokens: 50},
			{Name: "history", Tokens: 25},
		},
	}
	bar := contextBar(u, 20)
	blocks := strings.Count(bar, "█")
	tracks := strings.Count(bar, "░")
	if blocks != 15 { // (50*20/100) + (25*20/100) = 10 + 5
		t.Errorf("filled segments = %d, want 15", blocks)
	}
	if blocks+tracks < 20 {
		t.Errorf("bar total drawn width = %d, want at least 20 (width)", blocks+tracks)
	}
}

func TestContextBarEmptyBudgetDrawsOnlyTrack(t *testing.T) {
	bar := contextBar(daemonclient.ContextUsage{}, 10)
	if strings.Contains(bar, "█") {
		t.Errorf("expected no filled segments for a zero-budget usage, got %q", bar)
	}
}

func TestContextBarEnforcesMinimumWidth(t *testing.T) {
	// width < 10 should be clamped to 10, not produce a degenerate bar.
	u := daemonclient.ContextUsage{Budget: 10, Categories: []daemonclient.ContextCategory{{Name: "x", Tokens: 10}}}
	bar := contextBar(u, 1)
	if strings.Count(bar, "█") != 10 {
		t.Errorf("expected the 1-wide request to be clamped to width 10, got %d filled cells", strings.Count(bar, "█"))
	}
}

func TestProviderKindForEnvVarKnownAccount(t *testing.T) {
	kind, baseURL, ok := providerKindForEnvVar("OPENAI_API_KEY")
	if !ok || kind != "openai-compat" {
		t.Errorf("providerKindForEnvVar(OPENAI_API_KEY) = (%q, %q, %v), want kind=openai-compat ok=true", kind, baseURL, ok)
	}
}

func TestProviderKindForEnvVarUnknown(t *testing.T) {
	_, _, ok := providerKindForEnvVar("NOT_A_REAL_ENV_VAR")
	if ok {
		t.Error("expected ok=false for an env var not in providercatalog.Providers")
	}
}

func TestAccountByIDFindsKnownAccount(t *testing.T) {
	acc := accountByID("anthropic")
	if acc == nil || acc.EnvVar != "ANTHROPIC_API_KEY" {
		t.Errorf("accountByID(anthropic) = %+v, want EnvVar=ANTHROPIC_API_KEY", acc)
	}
}

func TestAccountByIDReturnsNilForUnknown(t *testing.T) {
	if accountByID("does-not-exist") != nil {
		t.Error("expected nil for an unknown account ID")
	}
}

func TestWizardGatewayModeLineThresholds(t *testing.T) {
	cases := []struct {
		name        string
		rows        []accountStatus
		customCount int
		want        string // substring expected in the output
	}{
		{"none configured", nil, 0, "no provider"},
		{"exactly one", []accountStatus{{envSet: true}}, 0, "BASIC"},
		{"two via custom", nil, 2, "RESILIENT"},
		{"one env plus one custom", []accountStatus{{storedSet: true}}, 1, "RESILIENT"},
	}
	for _, tc := range cases {
		got := wizardGatewayModeLine(tc.rows, tc.customCount)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: wizardGatewayModeLine(...) = %q, want it to contain %q", tc.name, got, tc.want)
		}
	}
}

func TestPingDotIdleWhenNotConfigured(t *testing.T) {
	got := pingDot(Model{}, "ANTHROPIC_API_KEY", false)
	if !strings.Contains(got, "○") {
		t.Errorf("pingDot(unconfigured) = %q, want the idle dot", got)
	}
}

func TestPingDotPingingShowsWarnDot(t *testing.T) {
	m := Model{accountsPinging: true}
	got := pingDot(m, "ANTHROPIC_API_KEY", true)
	if !strings.Contains(got, "◉") {
		t.Errorf("pingDot(pinging) = %q, want the in-flight dot", got)
	}
}

func TestPingDotReflectsResultStatus(t *testing.T) {
	cases := []struct {
		status providerping.Status
		want   string
	}{
		{providerping.StatusOK, "●"},
		{providerping.StatusDegraded, "●"},
		{providerping.StatusDown, "●"},
	}
	for _, tc := range cases {
		m := Model{accountsPings: map[string]providerping.Result{"K": {Status: tc.status}}}
		got := pingDot(m, "K", true)
		if !strings.Contains(got, tc.want) {
			t.Errorf("status=%v: pingDot(...) = %q, want it to contain %q", tc.status, got, tc.want)
		}
	}
}

func TestPingDetailEmptyWhenNoResult(t *testing.T) {
	if got := pingDetail(Model{}, "K"); got != "" {
		t.Errorf("pingDetail(no result) = %q, want empty", got)
	}
}

func TestPingDetailPrefersExplicitDetailOverLatency(t *testing.T) {
	m := Model{accountsPings: map[string]providerping.Result{"K": {Detail: "sem cota (429)", Latency: 50 * time.Millisecond}}}
	if got := pingDetail(m, "K"); got != "sem cota (429)" {
		t.Errorf("pingDetail(...) = %q, want the explicit detail string", got)
	}
}

func TestBadgeForProviderNoDataYet(t *testing.T) {
	got := badgeForProvider(statusclient.Provider{}, "p1")
	if !strings.Contains(got, "no data") {
		t.Errorf("badgeForProvider(zero value) = %q, want the no-data message", got)
	}
}

func TestBadgeForProviderBreakerOpen(t *testing.T) {
	got := badgeForProvider(statusclient.Provider{ID: "p1", BreakerOpen: true}, "p1")
	if !strings.Contains(got, "open") {
		t.Errorf("badgeForProvider(breaker open) = %q, want it to say \"open\"", got)
	}
}

func TestBadgeForProviderUnstableWhenSuccessRateBelowOne(t *testing.T) {
	got := badgeForProvider(statusclient.Provider{
		ID: "p1", Stats: statusclient.ProviderStats{Requests: 10, SuccessRate: 0.8},
	}, "p1")
	if !strings.Contains(got, "unstable") {
		t.Errorf("badgeForProvider(partial failures) = %q, want it to say \"unstable\"", got)
	}
}

func TestBadgeForProviderHealthyClosed(t *testing.T) {
	got := badgeForProvider(statusclient.Provider{
		ID: "p1", Stats: statusclient.ProviderStats{Requests: 10, SuccessRate: 1, AvgLatencyMS: 250},
	}, "p1")
	if !strings.Contains(got, "closed") || !strings.Contains(got, "250ms") || !strings.Contains(got, "100% success") {
		t.Errorf("badgeForProvider(healthy) = %q, want closed/250ms/100%% success", got)
	}
}

func TestSuggestedProjectsRootJoinsHome(t *testing.T) {
	got := suggestedProjectsRoot("/home/x")
	if !strings.HasPrefix(got, "/home/x") || !strings.Contains(got, "Projects") {
		t.Errorf("suggestedProjectsRoot(/home/x) = %q, want it under /home/x and named Projects", got)
	}
}

func TestSuggestedProjectsRootDefaultsWhenHomeEmpty(t *testing.T) {
	got := suggestedProjectsRoot("")
	if got == "" {
		t.Error("suggestedProjectsRoot(\"\") should still return a usable relative path")
	}
}

func TestExpandTildeExpandsHomeReference(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	if got := expandTilde("~/projects"); got != "/home/testuser/projects" {
		t.Errorf("expandTilde(~/projects) = %q, want /home/testuser/projects", got)
	}
	if got := expandTilde("~"); got != "/home/testuser" {
		t.Errorf("expandTilde(~) = %q, want /home/testuser", got)
	}
}

func TestExpandTildeLeavesNonTildePathsUnchanged(t *testing.T) {
	if got := expandTilde("/already/absolute"); got != "/already/absolute" {
		t.Errorf("expandTilde(/already/absolute) = %q, want it unchanged", got)
	}
}

func TestLastAssistantTokensEmptyWhenNoUsage(t *testing.T) {
	if got := (Model{}).lastAssistantTokens(); got != "" {
		t.Errorf("lastAssistantTokens(zero usage) = %q, want empty", got)
	}
}

func TestLastAssistantTokensFormatsRealUsage(t *testing.T) {
	m := Model{lastUsage: openai.Usage{TotalTokens: 1234}}
	if got := m.lastAssistantTokens(); got != "1234 tok" {
		t.Errorf("lastAssistantTokens(...) = %q, want %q", got, "1234 tok")
	}
}

// clearAllCatalogProviderEnvVars deterministically clears every env var
// providercatalog.Accounts knows about, so wizardHasProvider/
// wizardHavePaidProvider (which read the real process environment via
// accountRows) don't depend on whatever happens to be set on the host
// running the tests.
func clearAllCatalogProviderEnvVars(t *testing.T) {
	t.Helper()
	for _, a := range providercatalog.Accounts {
		t.Setenv(a.EnvVar, "")
	}
}

func TestWizardHasProviderFalseWithNothingConfigured(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	m := &Model{}
	if m.wizardHasProvider() {
		t.Error("expected false with no env vars set, no stored credentials, and no custom providers")
	}
}

func TestWizardHasProviderTrueWithOneEnvVarSet(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	m := &Model{}
	if !m.wizardHasProvider() {
		t.Error("expected true once one catalog account's env var is set")
	}
}

func TestWizardOperationalProviderAcceptsOKAndDegradedOnly(t *testing.T) {
	for _, status := range []providerping.Status{providerping.StatusOK, providerping.StatusDegraded} {
		m := &Model{accountsPings: map[string]providerping.Result{"K": {Status: status}}}
		if !m.wizardHasOperationalProvider() {
			t.Errorf("status %v should be operational", status)
		}
	}
	for _, status := range []providerping.Status{providerping.StatusUnknown, providerping.StatusDown} {
		m := &Model{accountsPings: map[string]providerping.Result{"K": {Status: status}}}
		if m.wizardHasOperationalProvider() {
			t.Errorf("status %v must not be operational", status)
		}
	}
}

func TestWizardProviderNormalContinueRequiresOperationalPing(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	t.Setenv("ANTHROPIC_API_KEY", "configured")
	m := Model{wizardMode: true, phase: phaseAccounts, accountsPings: map[string]providerping.Result{
		"ANTHROPIC_API_KEY": {Status: providerping.StatusDown},
	}}
	next, _ := m.handleAccountsKey(keyMsg("n"))
	got := next.(Model)
	if got.phase != phaseAccounts || !got.wizardProviderOverrideVisible {
		t.Fatalf("ordinary n must stay on providers and expose a separate override: phase=%v override=%v", got.phase, got.wizardProviderOverrideVisible)
	}

	next, _ = got.handleAccountsKey(keyMsg("c"))
	got = next.(Model)
	if got.phase != phaseWizardRouting {
		t.Fatalf("explicit c override should advance, got phase %v", got.phase)
	}
}

func TestWizardProviderNormalContinueAcceptsDegraded(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	t.Setenv("ANTHROPIC_API_KEY", "configured")
	m := Model{wizardMode: true, phase: phaseAccounts, accountsPings: map[string]providerping.Result{
		"ANTHROPIC_API_KEY": {Status: providerping.StatusDegraded},
	}}
	next, _ := m.handleAccountsKey(keyMsg("n"))
	if got := next.(Model); got.phase != phaseWizardRouting {
		t.Fatalf("a provider that responded successfully but slowly should advance, got phase %v", got.phase)
	}
}

func TestWizardConfiguredProviderCountCountsProviders(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	// One shared OpenRouter key fans out to every openrouter-* catalog entry,
	// so the count reflects gateway providers, not distinct keys — matching
	// what Detect actually builds (and thus what autoStrategy sees).
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	if got := (Model{}).wizardConfiguredProviderCount(); got < 2 {
		t.Errorf("OpenRouter key should count as its several free providers, got %d", got)
	}
}

func TestWizardConfiguredProviderCountSingleProvider(t *testing.T) {
	clearAllCatalogProviderEnvVars(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test") // a single, distinct provider
	if got := (Model{}).wizardConfiguredProviderCount(); got != 1 {
		t.Errorf("a single Anthropic key should count as 1 provider, got %d", got)
	}
}
