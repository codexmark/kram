package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/breaker"
	"github.com/codexmark/kram/internal/config"
	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/provider"
	"github.com/codexmark/kram/internal/router"
	"github.com/codexmark/kram/internal/telemetry"
)

// quietProvider commits immediately (first delta) and then stays silent
// for a stretch before finishing — the shape of a reasoning model
// pausing mid-generation.
type quietProvider struct {
	id    string
	quiet time.Duration
}

func (p quietProvider) ID() string           { return p.id }
func (p quietProvider) Kind() string         { return "scripted" }
func (p quietProvider) SupportsImages() bool { return false }
func (p quietProvider) SupportsTools() bool  { return true }
func (p quietProvider) ChatCompletion(ctx context.Context, _ openai.ChatCompletionRequest) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent, 4)
	go func() {
		defer close(ch)
		ch <- provider.StreamEvent{Delta: "a"}
		time.Sleep(p.quiet)
		ch <- provider.StreamEvent{Delta: "b"}
		ch <- provider.StreamEvent{Done: true}
	}()
	return ch, nil
}

// TestStreamResponseEmitsKeepAliveWhileUpstreamIsQuiet: while a committed
// upstream attempt is alive but forwarding nothing, the SSE response must
// still carry bytes (comment lines) so the daemon's idle detection can
// tell healthy-quiet from dead.
func TestStreamResponseEmitsKeepAliveWhileUpstreamIsQuiet(t *testing.T) {
	original := streamKeepAliveInterval
	streamKeepAliveInterval = 15 * time.Millisecond
	t.Cleanup(func() { streamKeepAliveInterval = original })

	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{ID: "p", Kind: "fake"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"p"}}},
		DefaultCombo: "default",
	}
	ps := map[string]provider.Provider{"p": quietProvider{id: "p", quiet: 90 * time.Millisecond}}
	br := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(cfg, ps, br, tel)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, "", ps, rt, br, tel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	if !strings.Contains(out, ": keep-alive") {
		t.Fatalf("quiet stretch produced no keep-alive comments:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) && !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream did not finish normally after the quiet stretch:\n%s", out)
	}
}

// TestPeekIdleForGivesTheLastCandidateTheProviderBudget: fast give-up is
// only rational while another ranked candidate remains.
func TestPeekIdleForGivesTheLastCandidateTheProviderBudget(t *testing.T) {
	pt := 120 * time.Second
	if got := peekIdleFor(1, 3, pt); got != 0 {
		t.Errorf("first of three = %v, want 0 (router default)", got)
	}
	if got := peekIdleFor(3, 3, pt); got != pt {
		t.Errorf("last of three = %v, want the provider timeout %v", got, pt)
	}
	if got := peekIdleFor(1, 1, pt); got != pt {
		t.Errorf("single candidate = %v, want the provider timeout %v", got, pt)
	}
}
