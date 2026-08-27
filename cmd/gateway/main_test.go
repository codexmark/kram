package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freePort reserves and immediately releases a loopback TCP port, returning
// its number. Tests use it instead of the gateway's compiled-in default
// (20128) so a stray local process holding that port can't make the gateway
// fail to bind — see the port collision fixed in
// https://github.com/codexmark/kram/issues/89.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// writeGatewayConfig writes a minimal valid gateway config bound to an
// ephemeral free port and returns its path. A non-zero port is written
// deliberately: config.Load rewrites port 0 to the default 20128, which is
// exactly the collision these tests must avoid.
func writeGatewayConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(fmt.Sprintf("host: 127.0.0.1\nport: %d\nproviders:\n  - id: local\n    kind: openai-compat\n    base_url: http://127.0.0.1:1\n    key_optional: true\ncombos:\n  - id: default\n    providers: [local]\ndefault_combo: default\n", freePort(t)))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunReportsConfigLoadFailureBeforeStartingGateway(t *testing.T) {
	err := run(context.Background(), t.TempDir()+"/missing.yaml", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "loading config") {
		t.Fatalf("run error = %v, want contextual config-load failure", err)
	}
}

func TestRunLoadsValidConfigAndHonorsCancellation(t *testing.T) {
	path := writeGatewayConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, path, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("cancelled gateway run = %v", err)
	}
}

func TestRunMainParsesConfigFlag(t *testing.T) {
	path := writeGatewayConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runMain(ctx, []string{"-config", path}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := runMain(ctx, []string{"-unknown"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown flag unexpectedly accepted")
	}
}

func TestRunMainHelpSucceedsWithoutLoadingConfig(t *testing.T) {
	for _, helpFlag := range []string{"-h", "--help"} {
		var output bytes.Buffer
		if err := runMain(context.Background(), []string{helpFlag, "--config", t.TempDir() + "/missing.yaml"}, &output); err != nil {
			t.Fatalf("runMain(%q) error = %v", helpFlag, err)
		}
		if got := output.String(); !strings.Contains(got, "Usage of kram-gateway:") || strings.Contains(got, "level=ERROR") {
			t.Fatalf("runMain(%q) output = %q", helpFlag, got)
		}
	}
}

func TestRunMainInvalidFlagDoesNotLoadConfig(t *testing.T) {
	var output bytes.Buffer
	err := runMain(context.Background(), []string{"--unknown", "--config", t.TempDir() + "/missing.yaml"}, &output)
	if err == nil || strings.Contains(err.Error(), "loading config") {
		t.Fatalf("invalid flag error = %v, want parse error before config load", err)
	}
}

func TestMainExitDoesNotLogHelpAsAnError(t *testing.T) {
	var output bytes.Buffer
	if code := mainExit(context.Background(), []string{"--help"}, &output); code != 0 {
		t.Fatalf("mainExit help code = %d", code)
	}
	if got := output.String(); strings.Contains(got, "level=ERROR") {
		t.Fatalf("help logged as an error: %q", got)
	}

	output.Reset()
	if code := mainExit(context.Background(), []string{"--unknown"}, &output); code != 1 {
		t.Fatalf("mainExit invalid flag code = %d", code)
	}
	if got := output.String(); !strings.Contains(got, "level=ERROR") {
		t.Fatalf("invalid flag did not get an error log: %q", got)
	}
}
