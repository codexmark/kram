package kramhome

import (
	"path/filepath"
	"testing"
)

func TestDirAndPathUseXDGConfigHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, "kram-gateway") {
		t.Fatalf("dir=%q", dir)
	}
	path, err := Path("nested", "config.json")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "kram-gateway", "nested", "config.json") {
		t.Fatalf("path=%q", path)
	}
}
