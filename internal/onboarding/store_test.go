package onboarding

import "testing"

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestFreshStateNeedsSetup(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !s.NeedsSetup() {
		t.Error("a state with no saved file should need setup")
	}
}

func TestSaveRoundTrip(t *testing.T) {
	isolate(t)

	if err := Save(State{ProjectsRoot: "/home/x/projects", LastWorkspace: "/home/x/projects/foo"}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.NeedsSetup() {
		t.Error("a saved state should not need setup")
	}
	if reloaded.ProjectsRoot != "/home/x/projects" || reloaded.LastWorkspace != "/home/x/projects/foo" {
		t.Errorf("ProjectsRoot/LastWorkspace did not round-trip: %+v", reloaded)
	}
	if reloaded.Version != currentVersion {
		t.Errorf("Version = %d, want %d", reloaded.Version, currentVersion)
	}
}

func TestSaveProgressRetainsFieldsButStillNeedsSetup(t *testing.T) {
	isolate(t)

	if err := SaveProgress(State{ProjectsRoot: "/projects", LastWorkspace: "/projects/kram"}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.NeedsSetup() || reloaded.Completed {
		t.Fatalf("progress state must remain incomplete: %+v", reloaded)
	}
	if reloaded.Version != currentVersion || reloaded.ProjectsRoot != "/projects" || reloaded.LastWorkspace != "/projects/kram" {
		t.Errorf("progress metadata did not round-trip: %+v", reloaded)
	}
}

func TestSaveProgressReopensPreviouslyCompletedSetup(t *testing.T) {
	isolate(t)
	if err := Save(State{}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProgress(State{LastWorkspace: "/new"}); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := Load()
	if !reloaded.NeedsSetup() {
		t.Fatal("starting a new setup run must leave onboarding incomplete until Ready")
	}
}

func TestOlderVersionNeedsSetupAgain(t *testing.T) {
	isolate(t)
	if err := Save(State{}); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := Load()
	reloaded.Version = currentVersion - 1
	if !reloaded.NeedsSetup() {
		t.Error("a state saved under an older wizard version should need setup again")
	}
}
