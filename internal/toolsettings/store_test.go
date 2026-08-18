package toolsettings

import "testing"

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestEverythingEnabledByDefault(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.IsDisabled("bash") {
		t.Error("a fresh store should have nothing disabled")
	}
}

func TestToggleRoundTrip(t *testing.T) {
	isolate(t)
	s, _ := Load()

	if err := s.SetDisabled("bash", true); err != nil {
		t.Fatal(err)
	}
	if !s.IsDisabled("bash") {
		t.Error("bash should be disabled after SetDisabled(true)")
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsDisabled("bash") {
		t.Error("disabled state should persist across Load")
	}

	if err := reloaded.SetDisabled("bash", false); err != nil {
		t.Fatal(err)
	}
	if reloaded.IsDisabled("bash") {
		t.Error("bash should be enabled again after SetDisabled(false)")
	}

	again, _ := Load()
	if again.IsDisabled("bash") {
		t.Error("re-enabling should persist across Load")
	}
}

func TestDisabledReturnsACopy(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.SetDisabled("grep", true)

	d := s.Disabled()
	d["memory_write"] = true // mutate the returned map

	if s.IsDisabled("memory_write") {
		t.Error("mutating the map from Disabled() should not affect the store")
	}
}

func TestMultipleDisabledNamesShareOneNamespace(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.SetDisabled("bash", true)
	_ = s.SetDisabled("my-custom-skill", true) // a skill name, not a built-in tool

	reloaded, _ := Load()
	if !reloaded.IsDisabled("bash") || !reloaded.IsDisabled("my-custom-skill") {
		t.Error("tool and skill names should coexist in the same disabled set")
	}
}
