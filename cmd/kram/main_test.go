package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/codexmark/kram/internal/cli/app"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/daemon"
	"github.com/codexmark/kram/internal/providercatalog"
)

type immediateProgram struct {
	err error
}

func (p immediateProgram) Run() (tea.Model, error) { return nil, p.err }

func minimalConfig() *config.Config {
	return &config.Config{
		Host: "127.0.0.1", Port: 20128,
		Providers:    []config.ProviderConfig{{ID: "p", Kind: "openai-compat", BaseURL: "http://127.0.0.1", KeyOptional: true}},
		Combos:       []config.ComboConfig{{ID: "default", Providers: []string{"p"}}},
		DefaultCombo: "default",
	}
}

func TestHealthAndPortHelpers(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer healthy.Close()
	if err := waitHealthy(context.Background(), healthy.URL, time.Second); err != nil {
		t.Fatal(err)
	}

	notHealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer notHealthy.Close()
	if err := waitHealthy(context.Background(), notHealthy.URL, 120*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("non-OK health result = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitHealthy(ctx, "http://127.0.0.1:1", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled health result = %v", err)
	}
	if port, err := freePort(); err != nil || port <= 0 {
		t.Fatalf("freePort = %d, %v", port, err)
	}
}

func TestLoadAndFinalizeConfigHelpers(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	if cfg, ok, err := loadConfigIfExists(missing); err != nil || ok || cfg != nil {
		t.Fatalf("missing config = cfg=%v ok=%v err=%v", cfg, ok, err)
	}

	invalid := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("providers: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfigIfExists(invalid); err == nil || !strings.Contains(err.Error(), "loading gateway config") {
		t.Fatalf("invalid config error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(minimalConfig(), path); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := loadConfigIfExists(path)
	if err != nil || !ok || loaded.DefaultCombo != "default" {
		t.Fatalf("loaded config = %+v ok=%v err=%v", loaded, ok, err)
	}
	got, err := finalizeFileConfig(loaded, 4242)
	if err != nil || got.Host != "127.0.0.1" || got.Port != 4242 {
		t.Fatalf("explicit finalized config = %+v err=%v", got, err)
	}
	got, err = finalizeFileConfig(loaded, 0)
	if err != nil || got.Port <= 0 {
		t.Fatalf("ephemeral finalized config = %+v err=%v", got, err)
	}
}

func TestLoadOrDetectGatewayConfigPrecedence(t *testing.T) {
	isolateReconcileTest(t)
	logger := slog.New(slog.DiscardHandler)
	explicit := filepath.Join(t.TempDir(), "explicit.yaml")
	if err := config.Save(minimalConfig(), explicit); err != nil {
		t.Fatal(err)
	}
	got, err := loadOrDetectGatewayConfig(explicit, 4321, "round-robin", t.TempDir(), nil, logger)
	if err != nil || got.Port != 4321 || got.Combos[0].Strategy != "" {
		t.Fatalf("explicit precedence = %+v err=%v", got, err)
	}
	if _, err := loadOrDetectGatewayConfig(filepath.Join(t.TempDir(), "missing"), 0, "", t.TempDir(), nil, logger); err == nil {
		t.Fatal("missing explicit config unexpectedly loaded")
	}

	workspace := t.TempDir()
	workspacePath := filepath.Join(workspace, ".kram", "config.yaml")
	if err := config.Save(minimalConfig(), workspacePath); err != nil {
		t.Fatal(err)
	}
	got, err = loadOrDetectGatewayConfig("", 5555, "ignored", workspace, nil, logger)
	if err != nil || got.Port != 5555 {
		t.Fatalf("workspace config = %+v err=%v", got, err)
	}

	if err := os.Remove(workspacePath); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kram-gateway", "config.yaml")
	if err := config.Save(minimalConfig(), global); err != nil {
		t.Fatal(err)
	}
	got, err = loadOrDetectGatewayConfig("", 6666, "ignored", workspace, nil, logger)
	if err != nil || got.Port != 6666 {
		t.Fatalf("global config = %+v err=%v", got, err)
	}
}

func TestLoadOrDetectFallsBackToLiveProvider(t *testing.T) {
	isolateReconcileTest(t)
	provider := providercatalog.Providers[0]
	t.Setenv(provider.EnvVar, "test-key")
	got, err := loadOrDetectGatewayConfig("", 7777, "smart", t.TempDir(), nil, slog.New(slog.DiscardHandler))
	if err != nil || got.Port != 7777 || got.Combos[0].Strategy != "smart" {
		t.Fatalf("detected config = %+v err=%v", got, err)
	}
}

func TestRunRejectsUnusableWorkspaceBeforeStartingServices(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(runOptions{workspace: filepath.Join(file, "child"), gatewayConfigPath: "skip-wizard"})
	if err == nil || !strings.Contains(err.Error(), "creating") {
		t.Fatalf("run error = %v", err)
	}
}

func TestRunSurfacesExplicitConfigFailureAfterPreparingWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	err := run(runOptions{workspace: workspace, gatewayConfigPath: filepath.Join(t.TempDir(), "missing.yaml")})
	if err == nil || !strings.Contains(err.Error(), "loading gateway config") {
		t.Fatalf("run config error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, ".kram", "kram.log")); statErr != nil {
		t.Fatalf("run did not prepare its log before config loading: %v", statErr)
	}
}

func TestRunStartsRealGatewayAndDaemonThenHandsOffToCLI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()

	gwPort, _ := freePort()
	daemonPort, _ := freePort()
	cfg := minimalConfig()
	cfg.Port = gwPort
	cfg.Providers[0].BaseURL = upstream.URL
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := config.Save(cfg, path); err != nil {
		t.Fatal(err)
	}

	originalProgram := newProgram
	t.Cleanup(func() { newProgram = originalProgram })
	called := false
	newProgram = func(m app.Model) programRunner {
		called = true
		return immediateProgram{}
	}

	err := run(runOptions{
		workspace: workspace, gatewayConfigPath: path, combo: "default", title: "test session", maxTurns: 3,
		gatewayPort: gwPort, daemonPort: daemonPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("CLI handoff was not reached")
	}
	if _, err := os.Stat(filepath.Join(workspace, ".kram", "kram-daemon.db")); err != nil {
		t.Fatalf("daemon database was not created: %v", err)
	}
}

func TestRunWizardStagePersistsConfigurationAndContinues(t *testing.T) {
	isolateReconcileTest(t)
	for _, p := range providercatalog.Providers {
		t.Setenv(p.EnvVar, "")
	}
	t.Setenv(providercatalog.Providers[0].EnvVar, "test-key")
	workspace := t.TempDir()
	gwPort, _ := freePort()
	daemonPort, _ := freePort()

	originalWizard, originalProgram := runWizard, newProgram
	t.Cleanup(func() { runWizard, newProgram = originalWizard, originalProgram })
	runWizard = func(string, bool) (app.WizardResult, error) {
		return app.WizardResult{
			Workspace: workspace, ProjectsRoot: filepath.Dir(workspace), Strategy: "round-robin", PermPreset: "strict", BootSplashShown: true,
		}, nil
	}
	called := false
	newProgram = func(app.Model) programRunner { called = true; return immediateProgram{} }

	err := run(runOptions{workspace: workspace, setup: true, combo: "default", maxTurns: 2, gatewayPort: gwPort, daemonPort: daemonPort})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("wizard flow did not continue to Stage 2 CLI")
	}
	for _, name := range []string{"config.yaml", "permissions.json", "onboarding.json"} {
		path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "kram-gateway", name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("wizard did not persist %s: %v", name, err)
		}
	}
}

func TestRunSurfacesWizardAndCLIError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	originalWizard := runWizard
	t.Cleanup(func() { runWizard = originalWizard })
	runWizard = func(string, bool) (app.WizardResult, error) { return app.WizardResult{}, errors.New("wizard failed") }
	if err := run(runOptions{workspace: t.TempDir(), setup: true}); err == nil || !strings.Contains(err.Error(), "running setup wizard") {
		t.Fatalf("wizard error = %v", err)
	}
}

func TestRunMainParsesFlagsVersionAndWorkspaceExplicit(t *testing.T) {
	original := runKram
	t.Cleanup(func() { runKram = original })
	wantErr := errors.New("delegated")
	var got runOptions
	runKram = func(opts runOptions) error { got = opts; return wantErr }
	var stdout, stderr bytes.Buffer
	err := runMain([]string{
		"-workspace", "/w", "-config", "cfg", "-strategy", "smart", "-model", "m", "-session", "s", "-title", "t",
		"-max-turns", "7", "-gateway-port", "8", "-daemon-port", "9", "-setup",
	}, &stdout, &stderr)
	if !errors.Is(err, wantErr) || !got.workspaceExplicit || got.workspace != "/w" || got.maxTurns != 7 || got.gatewayPort != 8 || got.daemonPort != 9 || !got.setup {
		t.Fatalf("runMain opts=%+v err=%v", got, err)
	}

	runKram = func(opts runOptions) error { got = opts; return nil }
	if err := runMain(nil, &stdout, &stderr); err != nil || got.workspaceExplicit {
		t.Fatalf("default args opts=%+v err=%v", got, err)
	}
	stdout.Reset()
	if err := runMain([]string{"-version"}, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), version) {
		t.Fatalf("version output=%q err=%v", stdout.String(), err)
	}
	if err := runMain([]string{"-max-turns", "bad"}, &stdout, &stderr); err == nil {
		t.Fatal("invalid integer flag unexpectedly accepted")
	}
}

func TestRunMainHelpSucceedsWithoutStartingKram(t *testing.T) {
	original := runKram
	t.Cleanup(func() { runKram = original })
	calls := 0
	runKram = func(runOptions) error {
		calls++
		return nil
	}

	for _, helpFlag := range []string{"-h", "--help"} {
		var stdout, stderr bytes.Buffer
		if err := runMain([]string{helpFlag}, &stdout, &stderr); err != nil {
			t.Fatalf("runMain(%q) error = %v", helpFlag, err)
		}
		if got := stderr.String(); !strings.Contains(got, "Usage of kram:") || strings.Contains(got, "kram: ") {
			t.Fatalf("runMain(%q) stderr = %q", helpFlag, got)
		}
		if stdout.Len() != 0 {
			t.Fatalf("runMain(%q) stdout = %q", helpFlag, stdout.String())
		}
	}
	if calls != 0 {
		t.Fatalf("Kram started %d times while printing help", calls)
	}
}

func TestRunMainInvalidFlagDoesNotStartKram(t *testing.T) {
	original := runKram
	t.Cleanup(func() { runKram = original })
	calls := 0
	runKram = func(runOptions) error {
		calls++
		return nil
	}

	if err := runMain([]string{"--max-turns", "bad"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid max-turns unexpectedly accepted")
	}
	if calls != 0 {
		t.Fatalf("Kram started %d times after invalid flag", calls)
	}
}

func TestMainExitDoesNotLogHelpAsAnError(t *testing.T) {
	original := runKram
	t.Cleanup(func() { runKram = original })
	runKram = func(runOptions) error {
		t.Fatal("Kram started while printing help")
		return nil
	}

	var stdout, stderr bytes.Buffer
	if code := mainExit([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("mainExit help code = %d", code)
	}
	if got := stderr.String(); strings.Contains(got, "kram: ") {
		t.Fatalf("help logged as an error: %q", got)
	}

	stderr.Reset()
	if code := mainExit([]string{"--unknown"}, &stdout, &stderr); code != 1 {
		t.Fatalf("mainExit invalid flag code = %d", code)
	}
	if got := stderr.String(); !strings.Contains(got, "kram: flag provided but not defined") {
		t.Fatalf("invalid flag did not get an error log: %q", got)
	}
}

func TestRunSurfacesServiceStartupFailures(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := config.Save(minimalConfig(), path); err != nil {
		t.Fatal(err)
	}
	originalGateway, originalDaemon := kramGatewayRun, kramDaemonRun
	originalWait, originalPort := kramWaitHealthy, kramFreePort
	t.Cleanup(func() {
		kramGatewayRun, kramDaemonRun = originalGateway, originalDaemon
		kramWaitHealthy, kramFreePort = originalWait, originalPort
	})
	blockGateway := func(ctx context.Context, _ *config.Config, _ *slog.Logger, _ *credentials.Store) error {
		<-ctx.Done()
		return nil
	}
	blockDaemon := func(ctx context.Context, _ daemon.Config, _ *slog.Logger) error { <-ctx.Done(); return nil }

	kramGatewayRun = func(context.Context, *config.Config, *slog.Logger, *credentials.Store) error {
		return errors.New("gateway boom")
	}
	kramDaemonRun = blockDaemon
	kramWaitHealthy = func(context.Context, string, time.Duration) error {
		time.Sleep(10 * time.Millisecond)
		return errors.New("unhealthy")
	}
	err := run(runOptions{workspace: workspace, gatewayConfigPath: path, gatewayPort: 23001, daemonPort: 23002})
	if err == nil || !strings.Contains(err.Error(), "gateway failed to start") {
		t.Fatalf("gateway startup error = %v", err)
	}

	kramGatewayRun = blockGateway
	kramDaemonRun = func(context.Context, daemon.Config, *slog.Logger) error { return errors.New("daemon boom") }
	healthCalls := 0
	kramWaitHealthy = func(context.Context, string, time.Duration) error {
		healthCalls++
		if healthCalls == 1 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
		return errors.New("unhealthy")
	}
	err = run(runOptions{workspace: workspace, gatewayConfigPath: path, gatewayPort: 23003, daemonPort: 23004})
	if err == nil || !strings.Contains(err.Error(), "daemon failed to start") {
		t.Fatalf("daemon startup error = %v", err)
	}

	kramGatewayRun, kramDaemonRun = blockGateway, blockDaemon
	kramWaitHealthy = func(context.Context, string, time.Duration) error { return errors.New("nothing listening") }
	err = run(runOptions{workspace: workspace, gatewayConfigPath: path, gatewayPort: 23005, daemonPort: 23006})
	if err == nil || !strings.Contains(err.Error(), "gateway didn't come up") {
		t.Fatalf("generic startup error = %v", err)
	}

	kramFreePort = func() (int, error) { return 0, errors.New("no ports") }
	err = run(runOptions{workspace: workspace, gatewayConfigPath: path, gatewayPort: 23007})
	if err == nil || !strings.Contains(err.Error(), "picking a daemon port") {
		t.Fatalf("daemon port error = %v", err)
	}
}
