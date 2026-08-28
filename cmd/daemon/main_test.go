package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/codexmark/kram/internal/daemon"
)

func TestDaemonConfigPreservesEntrypointFlags(t *testing.T) {
	cfg := daemonConfig("0.0.0.0", 42, "state.db", "http://gateway", "combo", "/workspace", 7, false, "", "")
	if cfg.Host != "0.0.0.0" || cfg.Port != 42 || cfg.DBPath != "state.db" || cfg.GatewayURL != "http://gateway" || cfg.Model != "combo" || cfg.Workspace != "/workspace" || cfg.MaxTurns != 7 || cfg.PreferStreaming {
		t.Fatalf("daemonConfig lost a flag value: %+v", cfg)
	}
}

func TestRunMainParsesFlagsAndDelegates(t *testing.T) {
	original := daemonRun
	t.Cleanup(func() { daemonRun = original })
	wantErr := errors.New("stopped")
	var got daemon.Config
	daemonRun = func(_ context.Context, cfg daemon.Config, _ *slog.Logger) error { got = cfg; return wantErr }
	err := runMain(context.Background(), []string{"-host", "0.0.0.0", "-port", "42", "-db", "x.db", "-gateway", "http://g", "-model", "m", "-workspace", "/w", "-max-turns", "9"}, &bytes.Buffer{})
	if !errors.Is(err, wantErr) || got.Port != 42 || got.MaxTurns != 9 || got.Workspace != "/w" {
		t.Fatalf("runMain cfg=%+v err=%v", got, err)
	}
	if !got.PreferStreaming {
		t.Fatalf("runMain cfg.PreferStreaming = false, want the -stream flag's default of true when not passed: %+v", got)
	}
	if err := runMain(context.Background(), []string{"-port", "bad"}, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid flag unexpectedly accepted")
	}
}

// TestRunMainStreamFlagDisablesPreferStreaming is the regression test
// for the concrete failure that motivated this flag: a local model
// whose server sends nothing at all during prompt prefill trips
// router.BoundedPeek's idle timeout on the streaming path, failing every
// turn — -stream=false must reach daemon.Config.PreferStreaming so a
// deployment stuck in that situation has a real way out.
func TestRunMainStreamFlagDisablesPreferStreaming(t *testing.T) {
	original := daemonRun
	t.Cleanup(func() { daemonRun = original })
	var got daemon.Config
	daemonRun = func(_ context.Context, cfg daemon.Config, _ *slog.Logger) error { got = cfg; return nil }
	if err := runMain(context.Background(), []string{"-stream=false"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got.PreferStreaming {
		t.Fatalf("runMain with -stream=false left cfg.PreferStreaming = true: %+v", got)
	}
}

func TestRunMainHelpSucceedsWithoutStartingDaemon(t *testing.T) {
	original := daemonRun
	t.Cleanup(func() { daemonRun = original })
	calls := 0
	daemonRun = func(context.Context, daemon.Config, *slog.Logger) error {
		calls++
		return nil
	}

	for _, helpFlag := range []string{"-h", "--help"} {
		var output bytes.Buffer
		if err := runMain(context.Background(), []string{helpFlag}, &output); err != nil {
			t.Fatalf("runMain(%q) error = %v", helpFlag, err)
		}
		if got := output.String(); !bytes.Contains(output.Bytes(), []byte("Usage of kram-daemon:")) || bytes.Contains(output.Bytes(), []byte("level=ERROR")) {
			t.Fatalf("runMain(%q) output = %q", helpFlag, got)
		}
	}
	if calls != 0 {
		t.Fatalf("daemon started %d times while printing help", calls)
	}
}

func TestRunMainInvalidFlagDoesNotStartDaemon(t *testing.T) {
	original := daemonRun
	t.Cleanup(func() { daemonRun = original })
	calls := 0
	daemonRun = func(context.Context, daemon.Config, *slog.Logger) error {
		calls++
		return nil
	}

	if err := runMain(context.Background(), []string{"--port", "bad"}, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid port unexpectedly accepted")
	}
	if calls != 0 {
		t.Fatalf("daemon started %d times after invalid flag", calls)
	}
}

func TestMainExitDoesNotLogHelpAsAnError(t *testing.T) {
	original := daemonRun
	t.Cleanup(func() { daemonRun = original })
	daemonRun = func(context.Context, daemon.Config, *slog.Logger) error {
		t.Fatal("daemon started while printing help")
		return nil
	}

	var output bytes.Buffer
	if code := mainExit(context.Background(), []string{"--help"}, &output); code != 0 {
		t.Fatalf("mainExit help code = %d", code)
	}
	if got := output.String(); bytes.Contains(output.Bytes(), []byte("level=ERROR")) {
		t.Fatalf("help logged as an error: %q", got)
	}

	output.Reset()
	if code := mainExit(context.Background(), []string{"--unknown"}, &output); code != 1 {
		t.Fatalf("mainExit invalid flag code = %d", code)
	}
	if got := output.String(); !bytes.Contains(output.Bytes(), []byte("level=ERROR")) {
		t.Fatalf("invalid flag did not get an error log: %q", got)
	}
}
