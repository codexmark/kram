// Command cli is the Kram CLI: a Bubble Tea chat interface over a
// kram-daemon session, with a live footer and an on-demand strategy panel
// backed by kram-gateway's real telemetry. It never persists anything or
// talks to an LLM provider itself — it's a view over the daemon and the
// gateway.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram-gateway/internal/cli/app"
	"github.com/codexmark/kram-gateway/internal/cli/daemonclient"
	"github.com/codexmark/kram-gateway/internal/cli/statusclient"
)

func main() {
	daemonURL := flag.String("daemon", "http://127.0.0.1:20130", "base URL of a running kram-daemon")
	gatewayURL := flag.String("gateway", "http://127.0.0.1:20128", "base URL of a running kram-gateway")
	sessionID := flag.String("session", "", "resume an existing session ID instead of starting a new one")
	title := flag.String("title", "", "title for a newly created session")
	combo := flag.String("model", "default", "gateway combo used for messages in this session")
	flag.Parse()

	if err := run(*daemonURL, *gatewayURL, *sessionID, *title, *combo); err != nil {
		fmt.Fprintln(os.Stderr, "kram:", err)
		os.Exit(1)
	}
}

func run(daemonURL, gatewayURL, sessionID, title, combo string) error {
	daemon := daemonclient.New(daemonURL)
	gateway := statusclient.New(gatewayURL)

	if sessionID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sess, err := daemon.CreateSession(ctx, title)
		if err != nil {
			return fmt.Errorf("could not reach kram-daemon at %s: %w", daemonURL, err)
		}
		sessionID = sess.ID
	}

	m := app.New(daemon, gateway, sessionID, combo)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
