package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunReportsConfigLoadFailureBeforeStartingGateway(t *testing.T) {
	err := run(context.Background(), t.TempDir()+"/missing.yaml", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "loading config") {
		t.Fatalf("run error = %v, want contextual config-load failure", err)
	}
}

func TestRunLoadsValidConfigAndHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("host: 127.0.0.1\nport: 0\nproviders:\n  - id: local\n    kind: openai-compat\n    base_url: http://127.0.0.1:1\n    key_optional: true\ncombos:\n  - id: default\n    providers: [local]\ndefault_combo: default\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, path, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("cancelled gateway run = %v", err)
	}
}

func TestRunMainParsesConfigFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("host: 127.0.0.1\nport: 0\nproviders:\n  - id: local\n    kind: openai-compat\n    base_url: http://127.0.0.1:1\n    key_optional: true\ncombos:\n  - id: default\n    providers: [local]\ndefault_combo: default\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
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
