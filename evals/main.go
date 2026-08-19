// Command evals runs scripted scenarios against Kram's real agent loop,
// through a real (or real-ish) configured provider, and checks specific
// behaviors — the thing unit tests structurally cannot check, since they
// exercise Kram's own code in isolation while an eval exercises what the
// model actually does when handed Kram's system prompt and tools.
//
// This directly operationalizes DECISIONS.md's definition of the gap it
// closes: "a harness that can answer 'did this prompt change make the
// agent better or worse'". Several scenarios here are regression tests
// for real bugs found by hand earlier in this project (the silent empty
// final answer, grep returning binary garbage, memory/skills never being
// used proactively) — the harness exists so the next regression of the
// same shape gets caught by `go run ./evals`, not by another hour of
// manual tmux debugging.
//
// Wiring mirrors cmd/kram: gateway and daemon run in-process on free
// localhost ports, auto-detected from whichever provider env var/stored
// credential is available — same autodetection providercatalog backs
// everywhere else, so an eval run uses exactly the setup a real user has.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/daemon"
	"github.com/codexmark/kram/internal/gateway"
	"github.com/codexmark/kram/internal/providercatalog"
)

func main() {
	os.Exit(run())
}

func run() int {
	gwCfg, err := detectGatewayConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "evals:", err)
		fmt.Fprintln(os.Stderr, "evals need a real provider configured — export an API key or add one via the accounts screen.")
		return 1
	}

	workspace, err := os.MkdirTemp("", "kram-evals-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "evals:", err)
		return 1
	}
	defer os.RemoveAll(workspace)

	// Gateway/daemon logs go to a file, same reason cmd/kram routes them
	// away from stdout — stdout here is the eval report, and interleaving
	// slog lines with it would make the report unreadable.
	logFile, err := os.Create(filepath.Join(workspace, "kram.log"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "evals:", err)
		return 1
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gwPort, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "evals:", err)
		return 1
	}
	gwCfg.Port = gwPort
	daemonPort, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "evals:", err)
		return 1
	}

	errCh := make(chan error, 2)
	go func() { errCh <- gateway.Run(ctx, &gwCfg, logger, nil) }()

	dbPath := filepath.Join(workspace, "eval.db")
	daemonCfg := daemon.Config{
		Host: "127.0.0.1", Port: daemonPort,
		DBPath:     dbPath,
		GatewayURL: fmt.Sprintf("http://127.0.0.1:%d", gwPort),
		Model:      "default", Workspace: workspace, MaxTurns: 20,
	}
	go func() { errCh <- daemon.Run(ctx, daemonCfg, logger) }()

	gatewayURL := fmt.Sprintf("http://127.0.0.1:%d", gwPort)
	daemonURL := fmt.Sprintf("http://127.0.0.1:%d", daemonPort)
	if err := waitHealthy(ctx, gatewayURL, 10*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "evals: gateway didn't come up:", err)
		return 1
	}
	if err := waitHealthy(ctx, daemonURL, 10*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "evals: daemon didn't come up:", err)
		return 1
	}

	client := daemonclient.New(daemonURL)
	env := &env{ctx: ctx, client: client, workspace: workspace}

	results := make([]result, 0, len(scenarios))
	for _, sc := range scenarios {
		fmt.Printf("running %-32s ", sc.name)
		res := runScenario(env, sc)
		results = append(results, res)
		fmt.Println(res.summary())
	}

	return report(results)
}

type scenario struct {
	name string
	// soft scenarios are printed and counted but never fail the run —
	// for behavior that depends on a specific model's capability level
	// (a free-tier model may just not be good enough to comply), where a
	// hard failure would make the harness useless the moment someone
	// runs it against a weak model rather than useful as a regression
	// signal against a fixed one.
	soft  bool
	setup func(env *env) error
	// check returns PASS/FAIL only when the scenario's property was
	// actually exercised; SKIP means the model never took the action the
	// check depends on observing, so nothing about the property in
	// question was verified either way — see verdict's doc comment.
	check func(env *env) (v verdict, detail string)
}

type env struct {
	ctx       context.Context
	client    *daemonclient.Client
	workspace string
}

// sendAndWait creates a fresh session, sends one message, and drains the
// SSE stream to the final result — the synchronous shape an eval needs,
// built on the same streaming client the real CLI uses.
func (e *env) sendAndWait(content string) (daemonclient.StreamEvent, []string, error) {
	sess, err := e.client.CreateSession(e.ctx, "eval")
	if err != nil {
		return daemonclient.StreamEvent{}, nil, err
	}
	stream, err := e.client.SendMessageStream(e.ctx, sess.ID, content, nil)
	if err != nil {
		return daemonclient.StreamEvent{}, nil, err
	}
	defer stream.Close()

	var toolsCalled []string
	for {
		evt, done, err := stream.Next()
		if err != nil {
			return daemonclient.StreamEvent{}, toolsCalled, err
		}
		if evt.Type == "tool_start" {
			toolsCalled = append(toolsCalled, evt.Name)
		}
		if evt.Type == "error" {
			return evt, toolsCalled, fmt.Errorf("agent error: %s", evt.Error)
		}
		if done {
			return evt, toolsCalled, nil
		}
	}
}

type result struct {
	scenario
	verdict verdict
	detail  string
	err     error
}

// summary renders one line for the report. A soft scenario's FAIL is
// labeled "soft-fail" so it's visually distinct from a hard failure at a
// glance, without changing what actually gates the run's exit code (see
// report). SKIP is printed as-is regardless of soft/hard — a hard
// scenario that somehow SKIPs is still worth seeing plainly, even though
// none of today's hard scenarios can (they're all deterministically
// exercised on every run).
func (r result) summary() string {
	status := string(r.verdict)
	if r.verdict == FailVerdict && r.soft {
		status = "soft-fail"
	}
	if r.err != nil {
		return fmt.Sprintf("%s (error: %v)", status, r.err)
	}
	return fmt.Sprintf("%s — %s", status, r.detail)
}

func runScenario(e *env, sc scenario) result {
	if sc.setup != nil {
		if err := sc.setup(e); err != nil {
			return result{scenario: sc, verdict: FailVerdict, err: fmt.Errorf("setup: %w", err)}
		}
	}
	v, detail := sc.check(e)
	return result{scenario: sc, verdict: v, detail: detail}
}

// report prints the final summary and decides the process exit code. Only
// a genuine FAIL on a hard (non-soft) scenario fails the run — a SKIP
// never does, hard or soft, since it means the property in question was
// never actually exercised rather than that it was exercised and found
// wanting. That's the entire point of the three-state verdict: a SKIP
// counted as a failure would punish the harness for the model's choice of
// which tools to call on a given prompt, and a SKIP counted as a pass
// (the bug this replaces) would silently understate how much was really
// verified.
func report(results []result) int {
	var hardFails int
	fmt.Println()
	fmt.Println("summary:")
	for _, r := range results {
		if r.verdict == FailVerdict && !r.soft {
			hardFails++
		}
		fmt.Printf("  %-32s %s\n", r.name, r.summary())
	}
	fmt.Println()
	if hardFails > 0 {
		fmt.Printf("%d hard scenario(s) failed\n", hardFails)
		return 1
	}
	fmt.Println("all hard scenarios passed")
	return 0
}

// --- provider autodetection, mirroring cmd/kram/autodetect.go ---

func detectGatewayConfig() (config.Config, error) {
	if store, err := credentials.Load(); err == nil {
		for envVar, key := range store.All() {
			if os.Getenv(envVar) == "" && key != "" {
				os.Setenv(envVar, key)
			}
		}
	}

	var providers []config.ProviderConfig
	var ids []string
	for _, p := range providercatalog.Providers {
		if os.Getenv(p.EnvVar) == "" {
			continue
		}
		providers = append(providers, config.ProviderConfig{
			ID: p.ID, Kind: p.Kind, BaseURL: p.BaseURL, APIKeyEnv: p.EnvVar,
			Model: p.DefaultModel, SupportsImages: p.SupportsImages, SupportsTools: p.SupportsTools,
		})
		ids = append(ids, p.ID)
	}
	if len(providers) == 0 {
		return config.Config{}, fmt.Errorf("no provider configured (tried: %s)", strings.Join(providercatalog.EnvVars(), ", "))
	}
	return config.Config{
		Host: "127.0.0.1", Providers: providers,
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "round-robin", Providers: ids}},
		DefaultCombo: "default",
	}, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitHealthy(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for %s/health", baseURL)
}
