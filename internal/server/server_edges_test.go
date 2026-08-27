package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/breaker"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/provider"
	"github.com/codexmark/kram/internal/router"
	"github.com/codexmark/kram/internal/telemetry"
)

func statusTestServer(t *testing.T, logger *slog.Logger) *Server {
	t.Helper()
	cfg := &config.Config{Providers: []config.ProviderConfig{{ID: "p", Kind: "fake"}}, Combos: []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"p"}}}, DefaultCombo: "default"}
	ps := map[string]provider.Provider{"p": scriptedProvider{id: "p"}}
	br := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(cfg, ps, br, tel)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, "", ps, rt, br, tel, logger)
}

func TestHealthStatusAndNotFoundHandlers(t *testing.T) {
	s := statusTestServer(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		path string
		want int
	}{{"/health", 200}, {"/admin/status", 200}, {"/missing", 404}} {
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if r.Code != tc.want {
			t.Fatalf("%s status=%d body=%s", tc.path, r.Code, r.Body.String())
		}
		if tc.want == 200 && r.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s content type", tc.path)
		}
	}
	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/admin/status", nil))
	var got statusResponse
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 || got.Providers[0].ID != "p" || got.Providers[0].Kind != "scripted" || len(got.Combos) != 1 || got.Combos[0].Strategy != "priority" || len(got.Strategies) == 0 {
		t.Fatalf("status=%#v", got)
	}
}

func TestSetStrategyIsLoopbackOnlyAndUpdatesStatus(t *testing.T) {
	s := statusTestServer(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := s.Handler()

	remote := httptest.NewRequest(http.MethodPost, "/admin/strategy", strings.NewReader(`{"combo":"default","strategy":"smart"}`))
	remote.RemoteAddr = "192.0.2.4:4321"
	remoteResult := httptest.NewRecorder()
	handler.ServeHTTP(remoteResult, remote)
	if remoteResult.Code != http.StatusForbidden {
		t.Fatalf("remote mutation status=%d body=%s", remoteResult.Code, remoteResult.Body.String())
	}

	local := httptest.NewRequest(http.MethodPost, "/admin/strategy", strings.NewReader(`{"combo":"default","strategy":"smart"}`))
	local.RemoteAddr = "127.0.0.1:4321"
	localResult := httptest.NewRecorder()
	handler.ServeHTTP(localResult, local)
	if localResult.Code != http.StatusOK {
		t.Fatalf("local mutation status=%d body=%s", localResult.Code, localResult.Body.String())
	}
	var updated statusCombo
	if err := json.Unmarshal(localResult.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.ID != "default" || updated.Strategy != "smart" {
		t.Fatalf("updated=%+v", updated)
	}

	bad := httptest.NewRequest(http.MethodPost, "/admin/strategy", strings.NewReader(`{"combo":"default","strategy":"not-real"}`))
	bad.RemoteAddr = "[::1]:4321"
	badResult := httptest.NewRecorder()
	handler.ServeHTTP(badResult, bad)
	if badResult.Code != http.StatusBadRequest {
		t.Fatalf("bad strategy status=%d body=%s", badResult.Code, badResult.Body.String())
	}
}

func TestSetStrategyPersistWritesConfigAndDefault(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	cfg := &config.Config{
		Host: "127.0.0.1", Port: 20999,
		Providers:    []config.ProviderConfig{{ID: "p", Kind: "anthropic"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"p"}}},
		DefaultCombo: "default",
	}
	if err := config.Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	// The live config the server holds carries an ephemeral runtime port (as
	// finalizeFileConfig would have rewritten it), which persist must NOT
	// clobber onto disk — the on-disk port is restored instead.
	live := *cfg
	live.Port = 41234
	ps := map[string]provider.Provider{"p": scriptedProvider{id: "p"}}
	br := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(&live, ps, br, tel)
	if err != nil {
		t.Fatal(err)
	}
	s := New(&live, path, ps, rt, br, tel, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodPost, "/admin/strategy", strings.NewReader(`{"combo":"default","strategy":"smart","persist":true,"make_default":true}`))
	req.RemoteAddr = "127.0.0.1:5555"
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("persist status=%d body=%s", res.Code, res.Body.String())
	}

	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Combos[0].Strategy != "smart" {
		t.Fatalf("persisted strategy=%q, want smart", saved.Combos[0].Strategy)
	}
	if saved.DefaultCombo != "default" {
		t.Fatalf("persisted default_combo=%q, want default", saved.DefaultCombo)
	}
	if saved.Port != 20999 {
		t.Fatalf("persist clobbered the on-disk port: got %d, want 20999", saved.Port)
	}
}

func TestSetStrategyPersistWithoutConfigFileIsUnavailable(t *testing.T) {
	// statusTestServer builds a server with configPath "" (no on-disk config).
	s := statusTestServer(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/admin/strategy", strings.NewReader(`{"combo":"default","strategy":"smart","persist":true}`))
	req.RemoteAddr = "127.0.0.1:5556"
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("persist without config status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestRecoveryAndWriteError(t *testing.T) {
	s := statusTestServer(t, nil)
	panicHandler := s.recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	r := httptest.NewRecorder()
	panicHandler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if r.Code != 500 || !strings.Contains(r.Body.String(), "boom") {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	r = httptest.NewRecorder()
	writeError(r, 418, "teapot")
	if r.Code != 418 || !strings.Contains(r.Body.String(), "teapot") || r.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("error response=%d %s", r.Code, r.Body.String())
	}
}

func TestDrainToBufferBranches(t *testing.T) {
	usage := &openai.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}
	calls := []openai.ToolCall{{ID: "c"}}
	ch := make(chan provider.StreamEvent, 3)
	ch <- provider.StreamEvent{Delta: "a", Usage: usage}
	ch <- provider.StreamEvent{Delta: "b", Done: true, ToolCalls: calls}
	ch <- provider.StreamEvent{Delta: "ignored"}
	close(ch)
	content, gotCalls, _, gotUsage, terminal, err := drainToBuffer(ch)
	if err != nil || content != "ab" || len(gotCalls) != 1 || gotUsage != usage || !terminal {
		t.Fatalf("drain=(%q,%#v,%#v,%v,%v)", content, gotCalls, gotUsage, terminal, err)
	}
	fail := make(chan provider.StreamEvent, 1)
	fail <- provider.StreamEvent{Err: errors.New("upstream")}
	close(fail)
	if _, _, _, _, _, err := drainToBuffer(fail); err == nil {
		t.Fatal("expected stream error")
	}
	abnormal := make(chan provider.StreamEvent)
	close(abnormal)
	if content, _, _, _, terminal, err := drainToBuffer(abnormal); err != nil || content != "" || terminal {
		t.Fatalf("abnormal close=(%q,%v,%v)", content, terminal, err)
	}
}

func TestWriteBufferedResponseStopAndToolCalls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls []openai.ToolCall
		want  string
	}{{"stop", nil, "stop"}, {"tools", []openai.ToolCall{{ID: "c"}}, "tool_calls"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRecorder()
			usage := &openai.Usage{TotalTokens: 4}
			writeBufferedResponse(r, "m", "p", "answer", tc.calls, nil, usage, []openai.AttemptInfo{{Provider: "p"}}, nil, "priority")
			var got openai.ChatCompletionResponse
			if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Provider != "p" || got.Choices[0].FinishReason != tc.want || got.Usage.TotalTokens != 4 || got.Strategy != "priority" {
				t.Fatalf("response=%#v", got)
			}
		})
	}
	r := httptest.NewRecorder()
	writeBufferedResponse(r, "m", "p", "", nil, nil, nil, nil, nil, "")
	var got openai.ChatCompletionResponse
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.TotalTokens != 0 {
		t.Fatalf("usage=%#v", got.Usage)
	}
}

type incapableProvider struct{ scriptedProvider }

func (incapableProvider) SupportsTools() bool  { return false }
func (incapableProvider) SupportsImages() bool { return false }

func TestChatHandlerValidationResolutionRankingAndBufferedSuccess(t *testing.T) {
	s := statusTestServer(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		name, body string
		want       int
		contains   string
	}{
		{"invalid json", "{", 400, "invalid JSON body"},
		{"empty messages", `{"model":"default","messages":[]}`, 400, "messages must not be empty"},
		{"buffered success", `{"model":"default","messages":[{"role":"user","content":"hi"}]}`, 200, `"provider":"p"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRecorder()
			s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body)))
			if r.Code != tc.want || !strings.Contains(r.Body.String(), tc.contains) {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
		})
	}

	cfg := &config.Config{Combos: []config.ComboConfig{{ID: "only", Providers: []string{"p"}}}}
	ps := map[string]provider.Provider{"p": scriptedProvider{id: "p"}}
	br := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(cfg, ps, br, tel)
	if err != nil {
		t.Fatal(err)
	}
	noDefault := New(cfg, "", ps, rt, br, tel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := httptest.NewRecorder()
	noDefault.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"missing","messages":[{"role":"user","content":"hi"}]}`)))
	if r.Code != 400 || !strings.Contains(r.Body.String(), "no combo matches") {
		t.Fatalf("resolve status=%d body=%s", r.Code, r.Body.String())
	}

	cfg = &config.Config{Combos: []config.ComboConfig{{ID: "only", Providers: []string{"p"}}}, DefaultCombo: "only"}
	ps = map[string]provider.Provider{"p": incapableProvider{scriptedProvider{id: "p"}}}
	br = breaker.NewRegistry()
	tel = telemetry.New()
	rt, err = router.New(cfg, ps, br, tel)
	if err != nil {
		t.Fatal(err)
	}
	ineligible := New(cfg, "", ps, rt, br, tel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r = httptest.NewRecorder()
	ineligible.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"only","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x"}}]}`)))
	if r.Code != 503 || !strings.Contains(r.Body.String(), "no eligible providers") {
		t.Fatalf("rank status=%d body=%s", r.Code, r.Body.String())
	}
}
