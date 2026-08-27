package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/cli/daemonclient"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/credentials"
	"github.com/codexmark/kram/internal/daemon"
	"github.com/codexmark/kram/internal/providercatalog"
)

func isolatedProviderEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, provider := range providercatalog.Providers {
		t.Setenv(provider.EnvVar, "")
	}
}

func TestEvalScenariosAgainstDeterministicDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"eval-session","title":"eval"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/sessions/eval-session/messages":
			var request struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			tool := ""
			switch {
			case strings.Contains(request.Content, "grep tool"):
				tool = "grep"
			case strings.Contains(request.Content, "zzz-eval"):
				tool = "skill_list"
			case strings.Contains(request.Content, "remember"), strings.Contains(request.Content, "prefer terse"):
				tool = "memory_write"
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if tool != "" {
				fmt.Fprintf(w, "data: {\"type\":\"tool_start\",\"name\":%q}\n\n", tool)
			}
			fmt.Fprint(w, "data: {\"type\":\"done\",\"message\":{\"content\":\"completed\"}}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/tools":
			var tools []map[string]string
			for _, name := range []string{"ask_question", "memory_write", "skill_list", "delegate_task", "run_background", "artifact_read", "session_search", "lsp_diagnostics", "snapshot_create"} {
				tools = append(tools, map[string]string{"Name": name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tools": tools, "skills": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	e := &env{ctx: context.Background(), client: daemonclient.New(server.URL, ""), workspace: t.TempDir()}
	for _, sc := range scenarios {
		got := runScenario(e, sc)
		if got.err != nil || got.verdict != PassVerdict {
			t.Errorf("scenario %s = %s err=%v detail=%s", sc.name, got.verdict, got.err, got.detail)
		}
	}
}

func TestSendAndWaitSurfacesStreamErrorAndSetupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sessions" {
			fmt.Fprint(w, `{"id":"s"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"tool_start\",\"name\":\"bash\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":\"boom\"}\n\n")
	}))
	defer server.Close()
	e := &env{ctx: context.Background(), client: daemonclient.New(server.URL, "")}
	_, tools, err := e.sendAndWait("fail")
	if err == nil || !strings.Contains(err.Error(), "boom") || len(tools) != 1 {
		t.Fatalf("sendAndWait error=%v tools=%v", err, tools)
	}

	sc := scenario{name: "setup", setup: func(*env) error { return errors.New("bad setup") }, check: func(*env) (verdict, string) {
		t.Fatal("check ran after setup failed")
		return PassVerdict, ""
	}}
	if got := runScenario(e, sc); got.err == nil || got.verdict != FailVerdict {
		t.Fatalf("setup failure result = %+v", got)
	}

	createFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusInternalServerError) }))
	defer createFail.Close()
	if _, _, err := (&env{ctx: context.Background(), client: daemonclient.New(createFail.URL, "")}).sendAndWait("x"); err == nil {
		t.Fatal("session creation failure was lost")
	}
	streamFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sessions" {
			fmt.Fprint(w, `{"id":"s"}`)
			return
		}
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer streamFail.Close()
	if _, _, err := (&env{ctx: context.Background(), client: daemonclient.New(streamFail.URL, "")}).sendAndWait("x"); err == nil {
		t.Fatal("stream creation failure was lost")
	}
}

func TestResultSummaryAndReportSemantics(t *testing.T) {
	if got := (result{scenario: scenario{soft: true}, verdict: FailVerdict, detail: "weak"}).summary(); !strings.Contains(got, "soft-fail") {
		t.Fatalf("soft summary = %q", got)
	}
	if got := (result{verdict: FailVerdict, err: errors.New("broken")}).summary(); !strings.Contains(got, "error: broken") {
		t.Fatalf("error summary = %q", got)
	}
	if code := report([]result{{scenario: scenario{name: "soft", soft: true}, verdict: FailVerdict}, {scenario: scenario{name: "skip"}, verdict: SkipVerdict}}); code != 0 {
		t.Fatalf("soft fail/skip report code = %d", code)
	}
	if code := report([]result{{scenario: scenario{name: "hard"}, verdict: FailVerdict}}); code != 1 {
		t.Fatalf("hard failure report code = %d", code)
	}
}

func TestProviderDetectionAndNetworkHelpers(t *testing.T) {
	isolatedProviderEnvironment(t)
	if _, err := detectGatewayConfig(); err == nil {
		t.Fatal("provider detection unexpectedly succeeded without credentials")
	}
	provider := providercatalog.Providers[0]
	t.Setenv(provider.EnvVar, "test-key")
	cfg, err := detectGatewayConfig()
	if err != nil || len(cfg.Providers) != 1 || cfg.Providers[0].ID != provider.ID {
		t.Fatalf("detected config = %+v err=%v", cfg, err)
	}
	if port, err := freePort(); err != nil || port <= 0 {
		t.Fatalf("freePort = %d, %v", port, err)
	}

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer healthy.Close()
	if err := waitHealthy(context.Background(), healthy.URL, time.Second); err != nil {
		t.Fatal(err)
	}
	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	unhealthy.Close()
	if err := waitHealthy(context.Background(), unhealthy.URL, 120*time.Millisecond); err == nil {
		t.Fatal("waitHealthy unexpectedly accepted a closed server")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitHealthy(cancelled, "http://127.0.0.1:1", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
}

func TestContains(t *testing.T) {
	if !contains([]string{"a", "b"}, "b") || contains([]string{"a"}, "z") {
		t.Fatal("contains returned the wrong membership result")
	}
}

func TestMainFailurePathIsRepresentedByRun(t *testing.T) {
	isolatedProviderEnvironment(t)
	if code := run(); code != 1 {
		t.Fatalf("run without provider = %d, want 1", code)
	}
}

func TestRunOrchestratesServicesAndReportsScenarios(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			fmt.Fprint(w, `{"id":"s"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/sessions/s/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"done\",\"message\":{\"content\":\"ok\"}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	originalDetect, originalPort := evalDetectGatewayConfig, evalFreePort
	originalWait, originalGateway, originalDaemon := evalWaitHealthy, evalGatewayRun, evalDaemonRun
	originalClient, originalScenarios := evalNewDaemonClient, evalScenarios
	t.Cleanup(func() {
		evalDetectGatewayConfig, evalFreePort = originalDetect, originalPort
		evalWaitHealthy, evalGatewayRun, evalDaemonRun = originalWait, originalGateway, originalDaemon
		evalNewDaemonClient, evalScenarios = originalClient, originalScenarios
	})

	evalDetectGatewayConfig = func() (config.Config, error) {
		return config.Config{Host: "127.0.0.1"}, nil
	}
	ports := []int{21001, 21002}
	evalFreePort = func() (int, error) { p := ports[0]; ports = ports[1:]; return p, nil }
	evalWaitHealthy = func(context.Context, string, time.Duration) error { return nil }
	evalGatewayRun = func(ctx context.Context, _ *config.Config, _ string, _ *slog.Logger, _ *credentials.Store) error {
		<-ctx.Done()
		return nil
	}
	evalDaemonRun = func(ctx context.Context, _ daemon.Config, _ *slog.Logger) error {
		<-ctx.Done()
		return nil
	}
	evalNewDaemonClient = func(string, string) *daemonclient.Client { return daemonclient.New(server.URL, "") }
	evalScenarios = []scenario{{name: "orchestration", check: func(e *env) (verdict, string) {
		evt, _, err := e.sendAndWait("hello")
		if err != nil || evt.Message.Content != "ok" {
			return FailVerdict, fmt.Sprintf("event=%+v err=%v", evt, err)
		}
		return PassVerdict, "ok"
	}}}
	if code := run(); code != 0 {
		t.Fatalf("orchestrated eval run = %d", code)
	}
}

func TestRunInfrastructureFailureBranches(t *testing.T) {
	originalDetect, originalPort := evalDetectGatewayConfig, evalFreePort
	originalWait := evalWaitHealthy
	originalGateway, originalDaemon := evalGatewayRun, evalDaemonRun
	t.Cleanup(func() {
		evalDetectGatewayConfig, evalFreePort, evalWaitHealthy = originalDetect, originalPort, originalWait
		evalGatewayRun, evalDaemonRun = originalGateway, originalDaemon
	})
	evalDetectGatewayConfig = func() (config.Config, error) { return config.Config{}, nil }
	evalFreePort = func() (int, error) { return 0, errors.New("no port") }
	if code := run(); code != 1 {
		t.Fatalf("gateway port failure = %d", code)
	}

	calls := 0
	evalFreePort = func() (int, error) {
		calls++
		if calls == 2 {
			return 0, errors.New("no daemon port")
		}
		return 22001, nil
	}
	if code := run(); code != 1 {
		t.Fatalf("daemon port failure = %d", code)
	}

	evalFreePort = func() (int, error) { return 22002, nil }
	evalGatewayRun = func(context.Context, *config.Config, string, *slog.Logger, *credentials.Store) error { return nil }
	evalDaemonRun = func(context.Context, daemon.Config, *slog.Logger) error { return nil }
	evalWaitHealthy = func(context.Context, string, time.Duration) error { return errors.New("unhealthy") }
	if code := run(); code != 1 {
		t.Fatalf("gateway health failure = %d", code)
	}
	healthCalls := 0
	evalWaitHealthy = func(context.Context, string, time.Duration) error {
		healthCalls++
		if healthCalls == 2 {
			return errors.New("daemon unhealthy")
		}
		return nil
	}
	if code := run(); code != 1 {
		t.Fatalf("daemon health failure = %d", code)
	}
}

func TestScenarioNegativeAndInconclusiveVerdicts(t *testing.T) {
	serve := func(event string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/sessions":
				fmt.Fprint(w, `{"id":"s"}`)
			case r.Method == http.MethodPost && r.URL.Path == "/sessions/s/messages":
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", event)
			case r.Method == http.MethodGet && r.URL.Path == "/tools":
				fmt.Fprint(w, `{"tools":[],"skills":[]}`)
			default:
				http.NotFound(w, r)
			}
		}))
	}

	empty := serve(`{"type":"done","message":{"content":""}}`)
	e := &env{ctx: context.Background(), client: daemonclient.New(empty.URL, ""), workspace: t.TempDir()}
	for _, sc := range scenarios {
		if sc.setup != nil {
			if err := sc.setup(e); err != nil {
				t.Fatal(err)
			}
		}
		v, _ := sc.check(e)
		if v == PassVerdict {
			t.Errorf("scenario %s unexpectedly passed an empty/no-tools daemon", sc.name)
		}
	}
	empty.Close()

	failing := serve(`{"type":"error","error":"model unavailable"}`)
	e.client = daemonclient.New(failing.URL, "")
	for _, sc := range scenarios[:5] { // the registry scenario uses /tools, not the stream
		v, _ := sc.check(e)
		if v != FailVerdict {
			t.Errorf("scenario %s stream error verdict = %s", sc.name, v)
		}
	}
	failing.Close()
}
