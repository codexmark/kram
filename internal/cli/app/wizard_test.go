package app

import (
	"testing"

	"github.com/codexmark/kram/internal/onboarding"
)

func TestWizardReadyIsTheOnlyStageTwoCompletionPoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := onboarding.SaveProgress(onboarding.State{ProjectsRoot: "/projects", LastWorkspace: "/projects/kram"}); err != nil {
		t.Fatal(err)
	}

	m := Model{
		phase:              phaseWizardSummary,
		workspace:          "/projects/kram",
		wizardProjectsRoot: "/projects",
	}
	next, cmd := m.handleWizardSummaryKey(keyMsg("enter"))
	got := next.(Model)
	if got.wizardCompletionErr != nil {
		t.Fatalf("Ready failed to persist completion: %v", got.wizardCompletionErr)
	}
	if cmd == nil {
		t.Fatal("Ready should proceed to create the welcome session after persisting completion")
	}

	state, err := onboarding.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.NeedsSetup() || !state.Completed {
		t.Fatalf("Ready must mark onboarding complete: %+v", state)
	}
	if state.ProjectsRoot != "/projects" || state.LastWorkspace != "/projects/kram" {
		t.Errorf("completion lost Stage 1 seed data: %+v", state)
	}
}

func TestWizardCustomToolsWaitsForLiveDaemonSyncBeforeAdvancing(t *testing.T) {
	m := Model{
		phase:                   phaseTools,
		wizardChosenToolsPreset: "custom",
	}
	next, cmd := m.handleToolsKey(keyMsg("esc"))
	got := next.(Model)
	if cmd == nil {
		t.Fatal("leaving Custom must issue a final live-daemon reconciliation")
	}
	if got.phase != phaseTools {
		t.Fatalf("phase = %v, want tools until daemon acknowledges settings", got.phase)
	}
	if !got.wizardToolSettingsPending {
		t.Fatal("custom exit must wait for toolSettingsUpdatedMsg")
	}
}
