package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGrantsMissingFileIsEmpty(t *testing.T) {
	gs, err := LoadGrants(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(gs.Rules()) != 0 {
		t.Errorf("a fresh workspace should have no grants")
	}
}

func TestGrantRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	gs, err := LoadGrants(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Add("bash", "git push origin feature/foo"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadGrants(workspace)
	if err != nil {
		t.Fatal(err)
	}
	rules := reloaded.Rules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 persisted grant, got %d", len(rules))
	}
	if rules[0].Tool != "bash" || rules[0].Pattern != "git push origin feature/foo" || rules[0].Decision != Allow {
		t.Errorf("unexpected grant round-tripped: %+v", rules[0])
	}
}

func TestGrantsScopedToWorkspace(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	gsA, _ := LoadGrants(a)
	_ = gsA.Add("bash", "git push origin main")

	gsB, _ := LoadGrants(b)
	if len(gsB.Rules()) != 0 {
		t.Error("a grant in workspace A must not leak into workspace B")
	}
}

func TestGrantFilePermissions(t *testing.T) {
	workspace := t.TempDir()
	gs, _ := LoadGrants(workspace)
	_ = gs.Add("bash", "echo hi")

	info, err := os.Stat(filepath.Join(workspace, ".kram", "permission_grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("grants file should be 0600, got %o", info.Mode().Perm())
	}
}
