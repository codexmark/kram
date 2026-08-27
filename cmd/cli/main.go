// Command cli is the Kram CLI: a Bubble Tea chat interface over a
// kram-daemon session, with a live footer and an on-demand strategy panel
// backed by kram-gateway's real telemetry. It never persists anything or
// talks to an LLM provider itself — it's a view over the daemon and the
// gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/app"
	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/cli/statusclient"
)

type programRunner interface {
	Run() (tea.Model, error)
}

var newProgram = func(m app.Model) programRunner {
	return tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
}

var runCLI = run

func main() {
	os.Exit(mainExit(os.Args[1:], os.Stderr))
}

func mainExit(args []string, stderr io.Writer) int {
	if err := runMain(args, stderr); err != nil {
		fmt.Fprintln(stderr, "kram:", err)
		return 1
	}
	return 0
}

func runMain(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("kram-cli", flag.ContinueOnError)
	fs.SetOutput(output)
	daemonURL := fs.String("daemon", "http://127.0.0.1:20130", "base URL of a running kram-daemon")
	gatewayURL := fs.String("gateway", "http://127.0.0.1:20128", "base URL of a running kram-gateway")
	sessionID := fs.String("session", "", "resume an existing session ID, skipping the picker")
	title := fs.String("title", "", "create a session with this title, skipping the picker")
	combo := fs.String("model", "default", "gateway combo used for messages in this session")
	workspace := fs.String("workspace", "", "project root shown on the picker banner (informational only — the daemon, not this process, enforces it)")
	authToken := fs.String("auth-token", "", "bearer token for the daemon (the value the daemon printed / wrote to its daemon.token file); required against a daemon started with auth")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	return runCLI(*daemonURL, *gatewayURL, *sessionID, *title, *combo, *workspace, *authToken)
}

func run(daemonURL, gatewayURL, sessionID, title, combo, workspace, authToken string) error {
	daemon := daemonclient.New(daemonURL, authToken)
	gateway := statusclient.New(gatewayURL)

	// An explicit -session or -title skips straight to chat/creation;
	// otherwise the CLI opens on the session picker.
	if sessionID == "" && title != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sess, err := daemon.CreateSession(ctx, title)
		if err != nil {
			return fmt.Errorf("could not reach kram-daemon at %s: %w", daemonURL, err)
		}
		sessionID = sess.ID
	}

	m := app.New(daemon, gateway, sessionID, combo, workspace, false, app.WizardResult{})
	p := newProgram(m)
	_, err := p.Run()
	return err
}
