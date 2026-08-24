package agent

import (
	"path/filepath"
	"testing"

	"github.com/codexmark/kram/internal/daemon/gatewayclient"
	"github.com/codexmark/kram/internal/daemon/store"
	"github.com/codexmark/kram/internal/daemon/tools"
)

// newTestRegistry builds a real, minimal tools.Registry for construction
// tests that don't need a full running agent — just a real tool universe
// to validate ToolOrder against.
func newTestRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate from any real global permissions.json
	return tools.NewRegistry(t.TempDir(), nil, nil)
}

func TestNewRejectsMalformedToolOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gatewayclient.New("http://127.0.0.1:1")
	tr := newTestRegistry(t)

	_, err = New(st, gw, tr, Config{ToolOrder: []string{"bash", "bash"}}) // no rest marker, plus a duplicate
	if err == nil {
		t.Fatal("expected New to reject a malformed ToolOrder")
	}
}

func TestNewRejectsUnregisteredToolOrderName(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gatewayclient.New("http://127.0.0.1:1")
	tr := newTestRegistry(t)

	_, err = New(st, gw, tr, Config{ToolOrder: []string{"not_a_real_tool", tools.ToolOrderRest}})
	if err == nil {
		t.Fatal("expected New to reject a ToolOrder naming an unregistered tool — this is the exact typo-vs-silently-vanishes bug the feature exists to prevent")
	}
}

func TestNewAcceptsWellFormedToolOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gatewayclient.New("http://127.0.0.1:1")
	tr := newTestRegistry(t)

	svc, err := New(st, gw, tr, Config{ToolOrder: []string{"bash", tools.ToolOrderRest}})
	if err != nil {
		t.Fatalf("expected a well-formed, all-registered ToolOrder to succeed, got %v", err)
	}
	if svc == nil {
		t.Fatal("expected a non-nil Service on success")
	}
}

func TestNewAllowsNilToolOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	gw := gatewayclient.New("http://127.0.0.1:1")
	tr := newTestRegistry(t)

	if _, err := New(st, gw, tr, Config{}); err != nil {
		t.Errorf("expected a nil ToolOrder (today's default) to always succeed, got %v", err)
	}
}
