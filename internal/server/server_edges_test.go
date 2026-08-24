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
	return New(cfg, ps, rt, br, tel, logger)
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
	if len(got.Providers) != 1 || got.Providers[0].ID != "p" || got.Providers[0].Kind != "scripted" || len(got.Combos) != 1 || got.Combos[0].Strategy != "priority" {
		t.Fatalf("status=%#v", got)
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
	noDefault := New(cfg, ps, rt, br, tel, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	ineligible := New(cfg, ps, rt, br, tel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r = httptest.NewRecorder()
	ineligible.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"only","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x"}}]}`)))
	if r.Code != 503 || !strings.Contains(r.Body.String(), "no eligible providers") {
		t.Fatalf("rank status=%d body=%s", r.Code, r.Body.String())
	}
}
