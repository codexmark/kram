// Command kram is the all-in-one entry point: it starts the gateway and
// daemon in-process (goroutines, not subprocesses) and drops straight
// into the CLI — no separate terminals, no manual port coordination. Each
// piece (cmd/gateway, cmd/daemon, cmd/cli) still works standalone for
// development or when you want them on separate machines; this is the
// version meant for actually using Kram day to day.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/app"
	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/cli/statusclient"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/daemon"
	"github.com/codexmark/kram/internal/gateway"
)

// version is set at build time via -ldflags "-X main.version=..."
// (see scripts/build-release.sh and .github/workflows/release.yml). A
// go build/go run with no ldflags — the everyday development path —
// leaves it at "dev", which is the honest answer for a binary that
// wasn't built through the release process.
var version = "dev"

func main() {
	workspace := flag.String("workspace", ".", "project root")
	gatewayConfigPath := flag.String("config", "", "path to a gateway config.yaml (auto-detected from known API-key env vars if omitted)")
	strategy := flag.String("strategy", "", "routing strategy for the auto-detected combo (priority, round-robin, prefix-affinity, smart, quality, fast, cheap, reliable, lkgp, p2c) — empty keeps the old default (priority order with a paid provider present, round-robin for free-tier-only). Ignored when -config points at a full gateway config.yaml, which sets strategy per combo instead.")
	combo := flag.String("model", "default", "gateway combo used for messages in this session")
	sessionID := flag.String("session", "", "resume an existing session ID instead of starting a new one")
	title := flag.String("title", "", "title for a newly created session")
	maxTurns := flag.Int("max-turns", 50, "iteration budget per agent run, tool round-trips included")
	gatewayPort := flag.Int("gateway-port", 0, "gateway port (0 = pick a free port)")
	daemonPort := flag.Int("daemon-port", 0, "daemon port (0 = pick a free port)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("kram", version)
		return
	}

	if err := run(runOptions{
		workspace: *workspace, gatewayConfigPath: *gatewayConfigPath, combo: *combo,
		sessionID: *sessionID, title: *title, maxTurns: *maxTurns,
		gatewayPort: *gatewayPort, daemonPort: *daemonPort, strategy: *strategy,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "kram:", err)
		os.Exit(1)
	}
}

type runOptions struct {
	workspace         string
	gatewayConfigPath string
	combo             string
	sessionID         string
	title             string
	maxTurns          int
	gatewayPort       int
	daemonPort        int
	strategy          string
}

func run(opts runOptions) error {
	absWorkspace, err := filepath.Abs(opts.workspace)
	if err != nil {
		return fmt.Errorf("resolving workspace: %w", err)
	}

	stateDir := filepath.Join(absWorkspace, ".kram")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", stateDir, err)
	}

	// Gateway/daemon logs go to a file, not stdout — stdout belongs to the
	// CLI's alt-screen once it starts, and interleaving would corrupt it.
	logFile, err := os.OpenFile(filepath.Join(stateDir, "kram.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, nil))

	// Load any keys saved via the CLI's accounts screen and export them
	// into this process's own environment before autodetection runs — a
	// real, already-exported env var always wins (checked first, never
	// overwritten), so this only fills gaps rather than overriding what
	// the user explicitly set in their shell.
	loadStoredCredentials()

	gwCfg, err := loadOrDetectGatewayConfig(opts.gatewayConfigPath, opts.gatewayPort, opts.strategy)
	if err != nil {
		return err
	}

	daemonPort := opts.daemonPort
	if daemonPort == 0 {
		daemonPort, err = freePort()
		if err != nil {
			return fmt.Errorf("picking a daemon port: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		cancel()
	}()

	errCh := make(chan error, 2)
	go func() { errCh <- gateway.Run(ctx, gwCfg, logger) }()

	daemonCfg := daemon.Config{
		Host: "127.0.0.1", Port: daemonPort,
		DBPath:     filepath.Join(stateDir, "kram-daemon.db"),
		GatewayURL: fmt.Sprintf("http://127.0.0.1:%d", gwCfg.Port),
		Model:      opts.combo, Workspace: absWorkspace, MaxTurns: opts.maxTurns,
	}
	go func() { errCh <- daemon.Run(ctx, daemonCfg, logger) }()

	gatewayURL := fmt.Sprintf("http://127.0.0.1:%d", gwCfg.Port)
	daemonURL := fmt.Sprintf("http://127.0.0.1:%d", daemonPort)

	if err := waitHealthy(ctx, gatewayURL, 5*time.Second); err != nil {
		return fmt.Errorf("gateway didn't come up: %w (see %s)", err, logFile.Name())
	}
	if err := waitHealthy(ctx, daemonURL, 5*time.Second); err != nil {
		return fmt.Errorf("daemon didn't come up: %w (see %s)", err, logFile.Name())
	}

	daemonC := daemonclient.New(daemonURL)
	gatewayC := statusclient.New(gatewayURL)

	// An explicit -session or -title skips the picker and goes straight to
	// chat/creation; otherwise the CLI opens on the session picker so a
	// durable session never gets buried behind "just start a new one".
	sid := opts.sessionID
	if sid == "" && opts.title != "" {
		createCtx, cancelCreate := context.WithTimeout(ctx, 10*time.Second)
		sess, err := daemonC.CreateSession(createCtx, opts.title)
		cancelCreate()
		if err != nil {
			return fmt.Errorf("creating session: %w", err)
		}
		sid = sess.ID
	}

	m := app.New(daemonC, gatewayC, sid, opts.combo, absWorkspace)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, cliErr := p.Run()

	cancel()
	// Give the gateway/daemon HTTP servers a moment to shut down cleanly.
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
	}

	return cliErr
}

// waitHealthy polls /health until it responds or the timeout elapses.
func waitHealthy(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("timed out waiting for %s/health", baseURL)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// loadOrDetectGatewayConfig loads an explicit config file if given,
// otherwise builds a minimal one from whichever well-known provider
// API-key env vars are actually set — so `kram` with no flags at all
// works the moment you've exported one key, and errors clearly if you
// haven't rather than silently doing nothing. strategyOverride, if set,
// replaces the auto-detected combo's strategy (see -strategy); ignored
// when an explicit -config file is given, since that file already picks
// its own strategy per combo.
func loadOrDetectGatewayConfig(path string, port int, strategyOverride string) (*config.Config, error) {
	if path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			return nil, fmt.Errorf("loading gateway config: %w", err)
		}
		if port != 0 {
			cfg.Port = port
		}
		return cfg, nil
	}

	cfg, err := detectGatewayConfig(strategyOverride)
	if err != nil {
		return nil, err
	}
	if port == 0 {
		port, err = freePort()
		if err != nil {
			return nil, fmt.Errorf("picking a gateway port: %w", err)
		}
	}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	return cfg, nil
}
