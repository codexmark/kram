package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestGoplsIntegration is a real end-to-end check against an actual gopls
// binary, skipped gracefully whenever one isn't installed — every other
// test in this package uses the fake in-process server and never
// requires this. Bonus coverage only, not part of the required suite.
func TestGoplsIntegration(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed, skipping real LSP integration test")
	}

	dir := t.TempDir()
	mainGo := `package main

import "fmt"

func greet() string {
	return "hi"
}

func main() {
	fmt.Println(greet())
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/kramlsp\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(dir)
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// greet() is called on the fmt.Println(greet()) line (line 9, 0
	// indexed) at roughly character 14, inside the call to greet.
	locs, err := m.Definition(ctx, "main.go", 9, 14)
	if err != nil {
		t.Fatalf("Definition via real gopls: %v", err)
	}
	if len(locs) == 0 {
		t.Fatal("expected gopls to resolve a definition for greet()")
	}
	if got := m.DisplayPath(locs[0].URI); got != "main.go" {
		t.Fatalf("expected the definition to resolve back into main.go, got %q", got)
	}
}
