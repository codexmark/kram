package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// runHeadless drives one turn without the TUI: it creates (or resumes) a
// session, sends prompt, and drains the SSE stream to stdout — the
// non-interactive path a CI job, script, or eval harness needs (`kram -p
// "..."`). The daemon and gateway are already running in-process by the
// time this is called (see run()); this is purely a different consumer of
// the same stream the TUI consumes.
//
// Two output shapes: plain text (assistant deltas to stdout, tool
// activity to stderr so stdout stays just the answer) and JSON (one event
// object per line on stdout, for machine consumption). Either way, a turn
// that ends in an "error" event returns a non-nil error so the process
// exits non-zero.
//
// There is no human to answer a mid-turn ask_question or approval prompt
// in headless mode, so both are auto-resolved safely: an approval is
// denied (a script must not be able to auto-approve a policy-gated action
// the operator never saw), a question is answered with a sentinel telling
// the model no interactive input is available. Both let the turn finish
// deterministically instead of blocking on input that will never come.
func runHeadless(ctx context.Context, client *daemonclient.Client, sessionID, prompt string, jsonOut bool, stdout, stderr io.Writer) error {
	if sessionID == "" {
		sess, err := client.CreateSession(ctx, headlessSessionTitle(prompt))
		if err != nil {
			return fmt.Errorf("creating session: %w", err)
		}
		sessionID = sess.ID
	}

	stream, err := client.SendMessageStream(ctx, sessionID, prompt, nil)
	if err != nil {
		return fmt.Errorf("sending message: %w", err)
	}
	defer stream.Close()

	enc := json.NewEncoder(stdout)
	wroteText := false
	for {
		evt, done, err := stream.Next()
		if err != nil {
			return fmt.Errorf("reading stream: %w", err)
		}

		if jsonOut {
			if evt.Type != "" {
				_ = enc.Encode(evt)
			}
		} else {
			switch evt.Type {
			case "delta":
				fmt.Fprint(stdout, evt.Content)
				wroteText = true
			case "tool_start":
				fmt.Fprintf(stderr, "· %s(%s)\n", evt.Name, evt.Args)
			case "notice":
				fmt.Fprintf(stderr, "· %s\n", evt.Text)
			case "done":
				// If no deltas streamed (buffered path), print the final
				// message content now so stdout still carries the answer.
				if !wroteText && evt.Message.Content != "" {
					fmt.Fprint(stdout, evt.Message.Content)
					wroteText = true
				}
			}
		}

		// Auto-resolve interactive prompts nobody can answer headless.
		switch evt.Type {
		case "approval":
			_ = client.AnswerApproval(ctx, sessionID, evt.ApprovalID, "deny")
		case "question":
			_ = client.AnswerQuestion(ctx, sessionID, evt.QuestionID, "(no interactive input available in headless mode)")
		}

		if evt.Type == "error" {
			return fmt.Errorf("agent error: %s", evt.Error)
		}
		if done {
			break
		}
	}

	if !jsonOut && wroteText {
		fmt.Fprintln(stdout) // one trailing newline so shell output isn't glued to the next prompt
	}
	return nil
}

// headlessSessionTitle derives a short session title from the prompt so a
// headless run still shows up meaningfully in the session picker later.
func headlessSessionTitle(prompt string) string {
	const max = 60
	runes := []rune(prompt)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	if len(runes) == 0 {
		return "headless"
	}
	return prompt
}
