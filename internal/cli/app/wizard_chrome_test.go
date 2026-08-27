package app

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/codexmark/kram/internal/providerping"
)

func TestWizardTrailWideShowsAllStepStates(t *testing.T) {
	wide := renderWizardTrail(3, 120)
	for _, want := range []string{"✓ Welcome", "✓ Projects", "▸ Providers", "○ Routing", "○ Ready"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide trail missing %q: %q", want, wide)
		}
	}
}

func TestWizardTrailNarrowDegradesToDotsWithPosition(t *testing.T) {
	narrow := renderWizardTrail(3, 60)
	if !strings.Contains(narrow, "step 3/8 · Providers") {
		t.Fatalf("narrow trail lost the position label: %q", narrow)
	}
	if strings.Contains(narrow, "○ Ready") {
		t.Fatalf("narrow trail should not spell out step names: %q", narrow)
	}
}

func TestRenderWizardFrameCentersWhenSizedAndNotBefore(t *testing.T) {
	m := Model{width: 100, height: 40}
	got := m.renderWizardFrame(2, "Projects", "body line", wizardKeysChoose, 0)
	first := strings.SplitN(got, "\n", 2)[0]
	if w := lipgloss.Width(first); w != 100 {
		t.Fatalf("sized frame not padded to terminal width: line width = %d, want 100", w)
	}
	for _, want := range []string{"Projects", "body line", "enter", "choose", "KRAM"} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame missing %q", want)
		}
	}

	unsized := Model{}
	got = unsized.renderWizardFrame(2, "Projects", "body line", wizardKeysChoose, 0)
	first = strings.SplitN(got, "\n", 2)[0]
	if w := lipgloss.Width(first); w >= 100 {
		t.Fatalf("unsized frame should not be padded to a fictitious terminal: line width = %d", w)
	}
}

func TestRenderWizardOptionsSelectedGetsBarAndOwnDescriptionLine(t *testing.T) {
	got := renderWizardOptions([]string{"One", "Two"}, []string{"first desc", "second desc"}, 1)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// Two options, two lines each, one blank separator.
	if len(lines) != 5 {
		t.Fatalf("options lines = %d, want 5: %q", len(lines), got)
	}
	if !strings.Contains(lines[3], "▌") || !strings.Contains(lines[3], "Two") {
		t.Fatalf("selected option lost its accent bar: %q", lines[3])
	}
	if !strings.Contains(lines[4], "▌") || !strings.Contains(lines[4], "second desc") {
		t.Fatalf("selected description must sit on its own barred line: %q", lines[4])
	}
	if strings.Contains(lines[0], "▌") {
		t.Fatalf("unselected option should not carry the bar: %q", lines[0])
	}
}

func TestWizardStepsRenderThroughSharedFrame(t *testing.T) {
	m := Model{width: 110, height: 40, wizardStep: 4}
	if got := m.renderWizardRouting(); !strings.Contains(got, "KRAM") || !strings.Contains(got, "▸ Routing") {
		t.Fatal("routing step is not rendering through the shared chrome")
	}
	m.wizardStep = 5
	if got := m.renderWizardPermissions(); !strings.Contains(got, "▸ Permissions") {
		t.Fatal("permissions step is not rendering through the shared chrome")
	}
}

func TestWizardSystemCheckEscGoesBackToTools(t *testing.T) {
	m := Model{phase: phaseWizardSystemCheck, wizardStep: 7}
	next, _ := m.handleWizardSystemCheckKey(keyMsg("esc"))
	got := next.(Model)
	if got.phase != phaseWizardToolsPreset || got.wizardStep != 6 {
		t.Fatalf("esc on Check = (phase %v, step %d), want tools preset step 6", got.phase, got.wizardStep)
	}
}

func TestWizardSummaryEscGoesBackToCheck(t *testing.T) {
	m := Model{phase: phaseWizardSummary, wizardStep: 8}
	next, _ := m.handleWizardSummaryKey(keyMsg("esc"))
	got := next.(Model)
	if got.phase != phaseWizardSystemCheck || got.wizardStep != 7 {
		t.Fatalf("esc on Ready = (phase %v, step %d), want system check step 7", got.phase, got.wizardStep)
	}
}

func TestAccountsWizardFooterMakesContinueExplicit(t *testing.T) {
	m := testModel(t)
	m.wizardMode = true

	// Nothing configured: the footer says exactly what unlocks continue.
	got := m.renderAccounts()
	if !strings.Contains(got, "connect a provider to unlock continue") {
		t.Fatalf("locked footer missing the unlock note: %q", got)
	}
	if strings.Contains(got, "continue to Routing") {
		t.Fatalf("continue CTA must not show before a provider works: %q", got)
	}

	// An operational provider: the continue CTA appears, named after the
	// next step, on its own line above the key bar.
	m.accountsPings = map[string]providerping.Result{"X": {Status: providerping.StatusOK}}
	got = m.renderAccounts()
	if !strings.Contains(got, "continue to Routing") {
		t.Fatalf("operational footer missing the explicit continue CTA: %q", got)
	}
	if !strings.Contains(got, "re-check") || !strings.Contains(got, "back") {
		t.Fatalf("key bar lost its context actions: %q", got)
	}
}
