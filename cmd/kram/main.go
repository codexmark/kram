// Command kram is the all-in-one entry point: it starts the gateway and
// daemon in-process (goroutines, not subprocesses) and drops straight
// into the CLI — no separate terminals, no manual port coordination. Each
// piece (cmd/gateway, cmd/daemon, cmd/cli) still works standalone for
// development or when you want them on separate machines; this is the
// version meant for actually using Kram day to day.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/daemon"
	"github.com/codexmark/kram/internal/gateway"
	"github.com/codexmark/kram/internal/gatewayconfig"
	"github.com/codexmark/kram/internal/kramhome"
	"github.com/codexmark/kram/internal/onboarding"
	"github.com/codexmark/kram/internal/permission"
)

// version is set at build time via -ldflags "-X main.version=..."
// (see scripts/build-release.sh and .github/workflows/release.yml). A
// go build/go run with no ldflags — the everyday development path —
// leaves it at "dev", which is the honest answer for a binary that
// wasn't built through the release process.
var version = "dev"

// programRunner is the narrow seam run needs from Bubble Tea. Keeping the
// constructor replaceable lets tests exercise the complete gateway/daemon
// orchestration without requiring a real TTY or weakening production setup.
type programRunner interface {
	Run() (tea.Model, error)
}

var newProgram = func(m app.Model) programRunner {
	return tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
}

var runWizard = app.RunWizard
var runKram = run
var kramGatewayRun = gateway.Run
var kramDaemonRun = daemon.Run
var kramWaitHealthy = waitHealthy
var kramFreePort = freePort

func main() {
	os.Exit(mainExit(os.Args[1:], os.Stdout, os.Stderr))
}

func mainExit(args []string, stdout, stderr io.Writer) int {
	if err := runMain(args, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "kram:", err)
		return 1
	}
	return 0
}

func runMain(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("kram", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workspace := fs.String("workspace", ".", "project root")
	gatewayConfigPath := fs.String("config", "", "path to a gateway config.yaml (auto-detected from known API-key env vars if omitted)")
	strategy := fs.String("strategy", "", "routing strategy for the auto-detected combo (priority, round-robin, prefix-affinity, smart, quality, fast, cheap, reliable, lkgp, p2c) — empty keeps the old default")
	combo := fs.String("model", "default", "gateway combo used for messages in this session")
	sessionID := fs.String("session", "", "resume an existing session ID instead of starting a new one")
	title := fs.String("title", "", "title for a newly created session")
	maxTurns := fs.Int("max-turns", 50, "model calls per automatic continuation segment (4 segments maximum)")
	gatewayPort := fs.Int("gateway-port", 0, "gateway port (0 = pick a free port)")
	daemonPort := fs.Int("daemon-port", 0, "daemon port (0 = pick a free port)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	setup := fs.Bool("setup", false, "re-run the first-run setup wizard even if it already completed")
	stream := fs.Bool("stream", true, "prefer the streaming gateway path (see daemon.Config.PreferStreaming's doc comment for the tradeoff); disable for a slow local model whose server sends nothing during prompt prefill, which can trip the streaming peek's idle timeout and fail every turn")
	prompt := fs.String("p", "", "headless mode: run this single prompt to completion, print the answer to stdout, and exit — no TUI (for CI, scripting, evals)")
	jsonOut := fs.Bool("json", false, "with -p, emit one JSON event object per line on stdout instead of plain text")
	maxContextTokens := fs.Int("max-context-tokens", 0, "override the model context-window budget that triggers history compaction (0 = derive from the active combo's smallest provider window, falling back to a conservative built-in default)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Fprintln(stdout, "kram", version)
		return nil
	}
	workspaceExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "workspace" {
			workspaceExplicit = true
		}
	})
	return runKram(runOptions{
		workspace: *workspace, workspaceExplicit: workspaceExplicit, gatewayConfigPath: *gatewayConfigPath, combo: *combo,
		sessionID: *sessionID, title: *title, maxTurns: *maxTurns,
		gatewayPort: *gatewayPort, daemonPort: *daemonPort, strategy: *strategy, setup: *setup, stream: *stream,
		prompt: *prompt, jsonOut: *jsonOut, stdout: stdout, maxContextTokens: *maxContextTokens,
	})
}

type runOptions struct {
	workspace         string
	workspaceExplicit bool
	gatewayConfigPath string
	combo             string
	sessionID         string
	title             string
	maxTurns          int
	gatewayPort       int
	daemonPort        int
	strategy          string
	setup             bool
	// prompt/jsonOut/stdout drive headless mode (-p): when prompt is
	// non-empty, run() runs one turn to completion and writes to stdout
	// instead of launching the TUI.
	prompt  string
	jsonOut bool
	stdout  io.Writer
	stream  bool
	// maxContextTokens overrides the compaction budget when > 0; otherwise
	// it's derived from the active combo's smallest provider context window.
	maxContextTokens int
}

func run(opts runOptions) error {
	// The wizard's trigger is purely onboarding state, not whether a
	// provider happens to already be configured — someone with
	// ANTHROPIC_API_KEY already exported still gets asked about routing/
	// permissions/tools once, otherwise they'd never see any of it. -setup
	// forces it back open regardless of Completed. Skipped entirely when
	// an explicit -config is given: that file already fully decides the
	// gateway's configuration, so there's nothing for the wizard to
	// produce.
	onboardState, _ := onboarding.Load() // zero value (NeedsSetup() == true) on any load error — same "missing file is normal" convention every local store here uses
	workspace := opts.workspace
	wizardStage1Completed := false
	var wizardResult app.WizardResult

	// Loaded once and threaded through everywhere a credential might be
	// needed for the rest of run(): gatewayconfig.Detect (to notice an
	// OAuth-only-connected account with no env var to autodetect from),
	// and gateway.Run (to actually resolve/refresh that account's token
	// on every request). nil on failure is fine — every use site already
	// guards for it, same as every other local store in this codebase.
	credStore, _ := credentials.Load()

	// Headless mode can't run the interactive setup wizard — there's no
	// TUI. Skip it; if no provider ends up configured, the gateway fails
	// to start below with a clear error, which is the right non-interactive
	// behavior.
	if (opts.setup || onboardState.NeedsSetup()) && opts.gatewayConfigPath == "" && opts.prompt == "" {
		result, err := runWizard(workspace, opts.workspaceExplicit)
		if err != nil {
			return fmt.Errorf("running setup wizard: %w", err)
		}
		wizardResult = result
		if !result.Cancelled {
			workspace = result.Workspace
			gatewayconfig.LoadStoredCredentials() // pick up whatever the wizard's provider step just saved, before building the config below

			cfgToSave, err := gatewayconfig.Detect(result.Strategy, credStore, nil) // no logger yet this early in run() — see the one further down for why nil is safe here
			if err != nil {
				return fmt.Errorf("building config after setup: %w", err)
			}
			// A safe default for a brand-new install, not something to
			// silently retrofit onto an existing hand-written config —
			// gatewayconfig.Detect never sets this on its own.
			for i := range cfgToSave.Combos {
				cfgToSave.Combos[i].Response = config.ResponseGateConfig{RejectEmpty: true, RequireTerminal: true}
			}
			cfgPath, err := kramhome.Path("config.yaml")
			if err != nil {
				return fmt.Errorf("resolving global config path: %w", err)
			}
			if err := config.Save(cfgToSave, cfgPath); err != nil {
				return fmt.Errorf("saving generated config: %w", err)
			}

			permPath, err := kramhome.Path("permissions.json")
			if err != nil {
				return fmt.Errorf("resolving global permissions path: %w", err)
			}
			pf := permission.RecommendedPolicy()
			switch result.PermPreset {
			case "strict":
				pf = permission.StrictPolicy()
			case "autonomous":
				pf = permission.AutonomousPolicy()
			}
			if err := permission.SavePolicy(pf, permPath); err != nil {
				return fmt.Errorf("saving generated permissions: %w", err)
			}

			// Stage 1 made the runtime startable, but setup is not complete
			// until the post-daemon Tools/System Check/Ready stages finish.
			// Persist the seed data with Completed=false so exiting anywhere
			// in Stage 2 reliably reopens setup on the next launch.
			if err := onboarding.SaveProgress(onboarding.State{ProjectsRoot: result.ProjectsRoot, LastWorkspace: workspace}); err != nil {
				return fmt.Errorf("saving setup progress: %w", err)
			}
			wizardStage1Completed = true
		}
		// Cancelled: fall through with the original workspace/opts
		// untouched — loadOrDetectGatewayConfig below reproduces exactly
		// today's "no provider configured" error if nothing was ever
		// saved, unchanged from before this wizard existed.
	}

	absWorkspace, err := filepath.Abs(workspace)
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
	// the user explicitly set in their shell. Harmless to repeat if the
	// wizard already called this above.
	gatewayconfig.LoadStoredCredentials()

	gwCfg, err := loadOrDetectGatewayConfig(opts.gatewayConfigPath, opts.gatewayPort, opts.strategy, absWorkspace, credStore, logger)
	if err != nil {
		return err
	}

	daemonPort := opts.daemonPort
	if daemonPort == 0 {
		daemonPort, err = kramFreePort()
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

	// credStore (loaded once, above) is passed straight through to
	// gateway.Run — not merged into os.Setenv like gatewayconfig.LoadStoredCredentials'
	// plain keys — because an oauth-authed provider's config entry
	// (auth_mode: oauth) needs the gateway to resolve and refresh its
	// credential live on every request, since a short-lived OAuth token
	// would otherwise go stale long before the gateway process exits.
	errCh := make(chan error, 2)
	gatewayRun := kramGatewayRun
	go func() { errCh <- gatewayRun(ctx, gwCfg, logger, credStore) }()

	// One auth token, generated here and shared between the in-process
	// daemon and the in-process CLI that talks to it — so the CLI is
	// authorized without a token-file round-trip, and every other local
	// process is not. See daemon.Config.AuthToken / server.guardMiddleware.
	daemonToken, err := newDaemonAuthToken()
	if err != nil {
		return err
	}

	daemonCfg := daemon.Config{
		Host: "127.0.0.1", Port: daemonPort,
		DBPath:     filepath.Join(stateDir, "kram-daemon.db"),
		GatewayURL: fmt.Sprintf("http://127.0.0.1:%d", gwCfg.Port),
		Model:      opts.combo, Workspace: absWorkspace, MaxTurns: opts.maxTurns,
		PreferStreaming: opts.stream,
		AuthToken:       daemonToken,
		// Derive the daemon→gateway timeout from the gateway's own provider
		// timeout and longest fallback chain, so a legitimate multi-provider
		// fallback round is never cut off client-side (see
		// config.Tunables.ResolvedGatewayClientTimeout).
		GatewayClientTimeout: gwCfg.Tunables.ResolvedGatewayClientTimeout(gwCfg.MaxComboLength()),
		// Size the compaction budget to the active combo's smallest provider
		// context window (a fallback can route to any of them), unless the
		// user pinned an explicit --max-context-tokens. 0 either way falls
		// back to compaction.DefaultMaxTokens inside the agent.
		MaxContextTokens: resolveMaxContextTokens(opts.maxContextTokens, gwCfg, opts.combo),
	}
	daemonRun := kramDaemonRun
	go func() { errCh <- daemonRun(ctx, daemonCfg, logger) }()

	gatewayURL := fmt.Sprintf("http://127.0.0.1:%d", gwCfg.Port)
	daemonURL := fmt.Sprintf("http://127.0.0.1:%d", daemonPort)

	if err := kramWaitHealthy(ctx, gatewayURL, 5*time.Second); err != nil {
		// waitHealthy only ever sees "nobody's listening yet" — the real
		// reason (e.g. "building provider %q: ...") already landed on
		// errCh the moment gateway.Run returned, well before this dial
		// ever timed out. Surface that instead of the generic connection-
		// refused message, which was hiding every real cause behind an
		// identical, useless error no matter what actually went wrong.
		select {
		case runErr := <-errCh:
			if runErr != nil {
				return fmt.Errorf("gateway failed to start: %w (see %s)", runErr, logFile.Name())
			}
		default:
		}
		return fmt.Errorf("gateway didn't come up: %w (see %s)", err, logFile.Name())
	}
	if err := kramWaitHealthy(ctx, daemonURL, 5*time.Second); err != nil {
		select {
		case runErr := <-errCh:
			if runErr != nil {
				return fmt.Errorf("daemon failed to start: %w (see %s)", runErr, logFile.Name())
			}
		default:
		}
		return fmt.Errorf("daemon didn't come up: %w (see %s)", err, logFile.Name())
	}

	daemonC := daemonclient.New(daemonURL, daemonToken)
	gatewayC := statusclient.New(gatewayURL)

	// Headless mode: run the one prompt to completion, print to stdout,
	// and return — no TUI. The daemon/gateway shut down via the deferred
	// cancel + drain below, same as the interactive path.
	if opts.prompt != "" {
		out := opts.stdout
		if out == nil {
			out = os.Stdout
		}
		headlessErr := runHeadless(ctx, daemonC, opts.sessionID, opts.prompt, opts.jsonOut, out, os.Stderr)
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
		return headlessErr
	}

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

	// app.New itself prioritizes an explicit sid over openOnToolsPreset, so
	// passing wizardStage1Completed here is safe even if -title also created a
	// session in the same run.
	m := app.New(daemonC, gatewayC, sid, opts.combo, absWorkspace, wizardStage1Completed, wizardResult)
	p := newProgram(m)
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

// newDaemonAuthToken returns a fresh random bearer token (128 bits, hex)
// for the in-process daemon<->CLI pair. Mirrors daemon.newAuthToken but
// stays here so the token is generated by cmd/kram and handed to both
// sides, rather than the daemon generating one the CLI can't see.
func newDaemonAuthToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating daemon auth token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// loadOrDetectGatewayConfig loads an explicit config file if given,
// otherwise builds a minimal one from whichever well-known provider
// API-key env vars are actually set — so `kram` with no flags at all
// works the moment you've exported one key, and errors clearly if you
// haven't rather than silently doing nothing. strategyOverride, if set,
// replaces the auto-detected combo's strategy (see -strategy); ignored
// when an explicit -config file is given, since that file already picks
// its own strategy per combo.
//
// Every branch that loads an existing file (explicit -config, workspace-
// local, or global) runs it through gatewayconfig.Reconcile first: a
// config.yaml written once by an earlier wizard run would otherwise stay
// frozen forever — a provider or account added via the Accounts UI
// afterward is invisible until the file is hand-edited or deleted. The
// pure-autodetect fallback below doesn't need this: it already calls
// gatewayconfig.Detect fresh on every boot, so there's nothing stale to
// reconcile against.
func loadOrDetectGatewayConfig(path string, port int, strategyOverride string, workspace string, credStore *credentials.Store, logger *slog.Logger) (*config.Config, error) {
	if path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			return nil, fmt.Errorf("loading gateway config: %w", err)
		}
		cfg = gatewayconfig.Reconcile(cfg, credStore, logger)
		if port != 0 {
			cfg.Port = port
		}
		return cfg, nil
	}

	// Two auto-discovered file tiers between an explicit -config and pure
	// env-var autodetection: a workspace-local config.yaml (a per-project
	// override someone can hand-write or, later, generate) beats the
	// wizard's own global one. Neither is the "I want a stable port"
	// signal an explicit -config is, so both get a fresh ephemeral port
	// (freePort) whenever -gateway-port wasn't explicitly given — reusing
	// whatever port happens to be written in the file would let two
	// workspace-local kram instances collide trying to bind the same one.
	if workspaceCfg, ok, err := loadConfigIfExists(filepath.Join(workspace, ".kram", "config.yaml")); err != nil {
		return nil, err
	} else if ok {
		return finalizeFileConfig(gatewayconfig.Reconcile(workspaceCfg, credStore, logger), port)
	}
	if globalPath, err := kramhome.Path("config.yaml"); err == nil {
		if globalCfg, ok, err := loadConfigIfExists(globalPath); err != nil {
			return nil, err
		} else if ok {
			return finalizeFileConfig(gatewayconfig.Reconcile(globalCfg, credStore, logger), port)
		}
	}

	cfg, err := gatewayconfig.Detect(strategyOverride, credStore, logger)
	if err != nil {
		return nil, err
	}
	return finalizeFileConfig(cfg, port)
}

// loadConfigIfExists loads path if it exists, reporting ok=false (not an
// error) when it simply doesn't — the normal case for both auto-
// discovered tiers, which are optional by nature.
func loadConfigIfExists(path string) (cfg *config.Config, ok bool, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		return nil, false, nil
	}
	cfg, err = config.Load(path)
	if err != nil {
		return nil, false, fmt.Errorf("loading gateway config %s: %w", path, err)
	}
	return cfg, true, nil
}

// resolveMaxContextTokens picks the compaction budget for the daemon: an
// explicit --max-context-tokens override wins; otherwise the active combo's
// smallest provider context window (a fallback chain can route to any of
// them, so the budget must fit the smallest), trying the requested combo
// first and the config's default_combo as a fallback. Returns 0 when nothing
// is known, which the agent turns into compaction.DefaultMaxTokens.
func resolveMaxContextTokens(override int, cfg *config.Config, combo string) int {
	if override > 0 {
		return override
	}
	if w := cfg.ComboContextWindow(combo); w > 0 {
		return w
	}
	if cfg.DefaultCombo != "" && cfg.DefaultCombo != combo {
		return cfg.ComboContextWindow(cfg.DefaultCombo)
	}
	return 0
}

func finalizeFileConfig(cfg *config.Config, port int) (*config.Config, error) {
	if port == 0 {
		var err error
		port, err = freePort()
		if err != nil {
			return nil, fmt.Errorf("picking a gateway port: %w", err)
		}
	}
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	return cfg, nil
}
