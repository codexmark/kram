package app

import "testing"

func TestSplashTimelineRevealsHoldsAndFades(t *testing.T) {
	reveal, fade := splashAnimation(0)
	if reveal != 0 || fade != 0 {
		t.Fatalf("initial animation = (%v, %v), want (0, 0)", reveal, fade)
	}

	reveal, fade = splashAnimation(splashRevealFrames)
	if reveal != 1 || fade != 0 {
		t.Fatalf("revealed animation = (%v, %v), want (1, 0)", reveal, fade)
	}

	reveal, fade = splashAnimation(splashRevealFrames + splashHoldFrames - 1)
	if reveal != 1 || fade != 0 {
		t.Fatalf("hold animation = (%v, %v), want (1, 0)", reveal, fade)
	}

	reveal, fade = splashAnimation(splashTotalFrames - 1)
	if reveal != 1 || fade != 1 {
		t.Fatalf("final animation = (%v, %v), want (1, 1)", reveal, fade)
	}
}

func TestSplashTransitionsToItsRealStartupScreen(t *testing.T) {
	m := Model{
		phase:        phaseSplash,
		splashTarget: phaseWizardEnvironment,
		splashFrame:  splashTotalFrames - 1,
		wizardMode:   true,
		ready:        true,
	}
	next, cmd := m.Update(splashTickMsg{})
	got := next.(Model)
	if got.phase != phaseWizardEnvironment {
		t.Fatalf("phase = %v, want wizard environment", got.phase)
	}
	if cmd == nil {
		t.Fatal("transition must initialize the destination screen")
	}
}

func TestSplashCanBeSkippedWithoutForwardingTheKey(t *testing.T) {
	m := Model{phase: phaseSplash, splashTarget: phasePicker}
	next, cmd := m.handleKey(keyMsg("enter"))
	got := next.(Model)
	if got.phase != phasePicker {
		t.Fatalf("phase = %v, want picker", got.phase)
	}
	if cmd == nil {
		t.Fatal("skipping must initialize the picker")
	}
	if got.titling {
		t.Fatal("skip key leaked into the picker")
	}
}

func TestNewShowsBootOnceAndPreservesItsDestination(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	normal := New(nil, nil, "", "auto", "/workspace", false, WizardResult{})
	if normal.phase != phaseSplash || normal.splashTarget != phasePicker {
		t.Fatalf("normal startup = phase %v target %v, want splash -> picker", normal.phase, normal.splashTarget)
	}

	afterWizard := New(nil, nil, "", "auto", "/workspace", true, WizardResult{BootSplashShown: true})
	if afterWizard.phase != phaseWizardToolsPreset {
		t.Fatalf("Stage 2 phase = %v, want tools preset without replaying splash", afterWizard.phase)
	}
}
