package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codexmark/kram/internal/cli/app"
)

type immediateProgram struct{ err error }

func (p immediateProgram) Run() (tea.Model, error) { return nil, p.err }

func TestRunWithTitleSurfacesDaemonFailureBeforeOpeningTUI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	err := run(srv.URL, "http://gateway.invalid", "", "new session", "default", "")
	if err == nil || !strings.Contains(err.Error(), "could not reach kram-daemon") {
		t.Fatalf("run error = %v, want daemon reachability context", err)
	}
}

func TestRunCreatesOptionalSessionAndReturnsProgramResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":"created","title":"new"}`)
	}))
	defer srv.Close()

	original := newProgram
	t.Cleanup(func() { newProgram = original })
	wantErr := errors.New("program stopped")
	calls := 0
	newProgram = func(app.Model) programRunner { calls++; return immediateProgram{err: wantErr} }
	if err := run(srv.URL, "http://gateway", "", "new", "default", t.TempDir()); !errors.Is(err, wantErr) {
		t.Fatalf("title run error = %v", err)
	}
	if err := run(srv.URL, "http://gateway", "existing", "", "default", t.TempDir()); !errors.Is(err, wantErr) {
		t.Fatalf("session run error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("program constructor calls = %d", calls)
	}
}

func TestRunMainParsesAllFlags(t *testing.T) {
	original := runCLI
	t.Cleanup(func() { runCLI = original })
	wantErr := errors.New("delegated")
	var got []string
	runCLI = func(daemon, gateway, session, title, model, workspace string) error {
		got = []string{daemon, gateway, session, title, model, workspace}
		return wantErr
	}
	err := runMain([]string{"-daemon", "d", "-gateway", "g", "-session", "s", "-title", "t", "-model", "m", "-workspace", "w"}, &bytes.Buffer{})
	if !errors.Is(err, wantErr) || strings.Join(got, ",") != "d,g,s,t,m,w" {
		t.Fatalf("runMain args=%v err=%v", got, err)
	}
	if err := runMain([]string{"-unknown"}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown flag unexpectedly accepted")
	}
}

func TestRunMainHelpSucceedsWithoutStartingCLI(t *testing.T) {
	original := runCLI
	t.Cleanup(func() { runCLI = original })
	calls := 0
	runCLI = func(string, string, string, string, string, string) error {
		calls++
		return nil
	}

	for _, helpFlag := range []string{"-h", "--help"} {
		var output bytes.Buffer
		if err := runMain([]string{helpFlag}, &output); err != nil {
			t.Fatalf("runMain(%q) error = %v", helpFlag, err)
		}
		if got := output.String(); !strings.Contains(got, "Usage of kram-cli:") || strings.Contains(got, "kram: ") {
			t.Fatalf("runMain(%q) output = %q", helpFlag, got)
		}
	}
	if calls != 0 {
		t.Fatalf("CLI started %d times while printing help", calls)
	}
}

func TestRunMainInvalidFlagDoesNotStartCLI(t *testing.T) {
	original := runCLI
	t.Cleanup(func() { runCLI = original })
	calls := 0
	runCLI = func(string, string, string, string, string, string) error {
		calls++
		return nil
	}

	var output bytes.Buffer
	if err := runMain([]string{"--unknown"}, &output); err == nil {
		t.Fatal("unknown flag unexpectedly accepted")
	}
	if calls != 0 {
		t.Fatalf("CLI started %d times after invalid flag", calls)
	}
}

func TestMainExitDoesNotLogHelpAsAnError(t *testing.T) {
	original := runCLI
	t.Cleanup(func() { runCLI = original })
	runCLI = func(string, string, string, string, string, string) error {
		t.Fatal("CLI started while printing help")
		return nil
	}

	var output bytes.Buffer
	if code := mainExit([]string{"--help"}, &output); code != 0 {
		t.Fatalf("mainExit help code = %d", code)
	}
	if got := output.String(); strings.Contains(got, "kram: ") {
		t.Fatalf("help logged as an error: %q", got)
	}

	output.Reset()
	if code := mainExit([]string{"--unknown"}, &output); code != 1 {
		t.Fatalf("mainExit invalid flag code = %d", code)
	}
	if got := output.String(); !strings.Contains(got, "kram: flag provided but not defined") {
		t.Fatalf("invalid flag did not get an error log: %q", got)
	}
}
