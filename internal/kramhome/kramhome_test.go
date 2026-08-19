package kramhome

import (
	"path/filepath"
	"testing"
)

func TestDirPrefersXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config/root")
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/custom/config/root", "kram-gateway")
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

func TestDirFallsBackToHomeConfigWhenXDGUnset(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/testuser")
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/testuser", ".config", "kram-gateway")
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// TestDirUsesGatewaySuffixNotShortName pins the deliberate choice
// documented on Dir() itself: "kram-gateway", never the shorter "kram",
// to avoid colliding with an unrelated tool of the same short name.
func TestDirUsesGatewaySuffixNotShortName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x")
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "kram-gateway" {
		t.Errorf("Dir() base = %q, want %q", filepath.Base(got), "kram-gateway")
	}
}

func TestPathJoinsDirWithGivenElements(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config/root")
	got, err := Path("credentials.json")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/custom/config/root", "kram-gateway", "credentials.json")
	if got != want {
		t.Errorf("Path(\"credentials.json\") = %q, want %q", got, want)
	}
}

func TestPathJoinsMultipleElements(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config/root")
	got, err := Path("skills", "my-skill", "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/custom/config/root", "kram-gateway", "skills", "my-skill", "SKILL.md")
	if got != want {
		t.Errorf("Path(...) = %q, want %q", got, want)
	}
}

func TestPathWithNoElementsReturnsBareDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config/root")
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir()
	if got != dir {
		t.Errorf("Path() with no elements = %q, want it to equal Dir() = %q", got, dir)
	}
}
