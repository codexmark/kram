package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/cli/daemonclient"
)

// headlessFakeDaemon serves the minimal daemon surface runHeadless needs:
// create a session, then stream a scripted SSE sequence for the message.
func headlessFakeDaemon(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sess_headless","title":"x"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sse))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestRunHeadlessTextModeWritesFinalAnswer(t *testing.T) {
	sse := "data: {\"type\":\"delta\",\"content\":\"Hello \"}\n\n" +
		"data: {\"type\":\"delta\",\"content\":\"world\"}\n\n" +
		"data: {\"type\":\"done\",\"message\":{\"content\":\"Hello world\"}}\n\n" +
		"data: [DONE]\n\n"
	srv := headlessFakeDaemon(t, sse)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	err := runHeadless(context.Background(), daemonclient.New(srv.URL, ""), "", "hi", false, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "Hello world" {
		t.Errorf("stdout = %q, want the streamed answer", got)
	}
}

func TestRunHeadlessJSONModeEmitsEventLines(t *testing.T) {
	sse := "data: {\"type\":\"tool_start\",\"name\":\"bash\",\"args\":\"ls\"}\n\n" +
		"data: {\"type\":\"delta\",\"content\":\"done\"}\n\n" +
		"data: {\"type\":\"done\",\"message\":{\"content\":\"done\"}}\n\n" +
		"data: [DONE]\n\n"
	srv := headlessFakeDaemon(t, sse)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if err := runHeadless(context.Background(), daemonclient.New(srv.URL, ""), "", "hi", true, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var evt struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &evt) == nil {
			types = append(types, evt.Type)
		}
	}
	if strings.Join(types, ",") != "tool_start,delta,done" {
		t.Errorf("JSON event types = %v, want tool_start,delta,done", types)
	}
}

func TestRunHeadlessErrorEventReturnsError(t *testing.T) {
	sse := "data: {\"type\":\"error\",\"error\":\"boom\"}\n\n" + "data: [DONE]\n\n"
	srv := headlessFakeDaemon(t, sse)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	err := runHeadless(context.Background(), daemonclient.New(srv.URL, ""), "", "hi", false, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want it to surface the agent error (non-zero exit)", err)
	}
}

func TestRunHeadlessAutoDeniesApproval(t *testing.T) {
	var approvedDecision string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sessions":
			_, _ = w.Write([]byte(`{"id":"s","title":"x"}`))
		case strings.HasSuffix(r.URL.Path, "/approve"):
			var body struct {
				Decision string `json:"decision"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			approvedDecision = body.Decision
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"approval\",\"approval_id\":\"a1\",\"tool\":\"bash\"}\n\n" +
				"data: {\"type\":\"done\",\"message\":{\"content\":\"ok\"}}\n\n" +
				"data: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	if err := runHeadless(context.Background(), daemonclient.New(srv.URL, ""), "", "hi", false, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if approvedDecision != "deny" {
		t.Errorf("headless approval decision = %q, want deny (a script must not auto-approve)", approvedDecision)
	}
}
