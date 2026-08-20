package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/onboarding"
)

func TestWizardEnvironmentSeparatesGitBinaryFromCurrentRepository(t *testing.T) {
	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	hasGit, cwdIsRepo := wizardEnvironment(t.TempDir())
	if !hasGit || cwdIsRepo {
		t.Fatalf("installed Git outside a repository = (%v, %v), want (true, false)", hasGit, cwdIsRepo)
	}

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	hasGit, cwdIsRepo = wizardEnvironment(repo)
	if hasGit || !cwdIsRepo {
		t.Fatalf("repository without Git on PATH = (%v, %v), want (false, true)", hasGit, cwdIsRepo)
	}
}

func TestNewWizardModelUsesRepositoryOnlyForWorkspaceDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	m := newWizardModel("./chosen", false)
	if !m.wizardMode || m.phase != phaseSplash || m.splashTarget != phaseWizardEnvironment {
		t.Fatalf("wizard did not start through its splash/environment flow: phase=%v target=%v", m.phase, m.splashTarget)
	}
	if got := m.wizardWorkspaceInput.Value(); got != repo {
		t.Fatalf("repository workspace default = %q, want %q", got, repo)
	}
	if m.wizardProjectsRootInput.Value() == repo {
		t.Fatal("projects root must remain independent from the current repository")
	}
}

func TestHandleRouteStartMarksRoutingAsRunning(t *testing.T) {
	m := Model{}
	next, _ := m.handleStreamEvent(streamEventMsg{event: daemonclient.StreamEvent{Type: "route_start"}})
	if !next.(Model).routeRunning {
		t.Fatal("route_start did not activate the live route rail")
	}
}

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
