package server

import (
	"context"
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

// scriptedProvider replays a fixed event script on every ChatCompletion
// call and then closes the channel — including, deliberately, scripts that
// never send a Done or Err event, to reproduce an upstream that closes its
// connection abnormally mid-stream.
type scriptedProvider struct {
	id     string
	events []provider.StreamEvent
}

func (p scriptedProvider) ID() string           { return p.id }
func (p scriptedProvider) Kind() string         { return "scripted" }
func (p scriptedProvider) SupportsImages() bool { return false }
func (p scriptedProvider) SupportsTools() bool  { return true }
func (p scriptedProvider) ChatCompletion(context.Context, openai.ChatCompletionRequest) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent, len(p.events))
	for _, e := range p.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// newStreamTestServer wires a two-provider "lkgp" combo: p1 is declared
// first, so plain priority order already ranks it first — the "lkgp"
// strategy on top of that only adds a "last-known-good" reason to
// whichever provider actually won a request (see lkgpStrategy.RecordOutcome),
// giving tests a simple, unambiguous signal for whether
// router.RecordOutcome(..., true) was actually called. p2 is never
// scripted to produce anything meaningful, since p1 always ranks first and
// these tests never exercise fallback.
func newStreamTestServer(t *testing.T, p1Events []provider.StreamEvent) (*Server, *router.Router) {
	t.Helper()
	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{ID: "p1", Kind: "fake"}, {ID: "p2", Kind: "fake"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "lkgp", Providers: []string{"p1", "p2"}}},
		DefaultCombo: "default",
	}
	providers := map[string]provider.Provider{
		"p1": scriptedProvider{id: "p1", events: p1Events},
		"p2": scriptedProvider{id: "p2"},
	}
	breakers := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(cfg, providers, breakers, tel)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, providers, rt, breakers, tel, logger), rt
}

const streamTestBody = `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func postStreamingChat(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamTestBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// terminalChunk parses an SSE response body and returns the one chunk that
// carries a FinishReason — the terminal chunk every test here actually
// cares about.
func terminalChunk(t *testing.T, body string) openai.ChatCompletionChunk {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk openai.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("parsing SSE chunk %q: %v", data, err)
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			return chunk
		}
	}
	t.Fatalf("no terminal chunk found in body:\n%s", body)
	return openai.ChatCompletionChunk{}
}

// rankReasons re-ranks the same request the tests always send and returns
// providerID's Reasons — used to check whether RecordOutcome actually
// reached the strategy (see newStreamTestServer's doc comment).
func rankReasons(t *testing.T, rt *router.Router, providerID string) []string {
	t.Helper()
	var req openai.ChatCompletionRequest
	if err := json.Unmarshal([]byte(streamTestBody), &req); err != nil {
		t.Fatalf("parsing test request: %v", err)
	}
	ranked, _, err := rt.Rank("default", req, "")
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	for _, rc := range ranked {
		if rc.Provider.Provider.ID() == providerID {
			return rc.Reasons
		}
	}
	t.Fatalf("provider %q not found in ranking", providerID)
	return nil
}

func TestStreamingSuccessOnlyRecordsWinAfterDone(t *testing.T) {
	s, rt := newStreamTestServer(t, []provider.StreamEvent{
		{Delta: "hello"},
		{Delta: " world", Done: true},
	})

	rec := postStreamingChat(t, s)
	chunk := terminalChunk(t, rec.Body.String())

	if got := *chunk.Choices[0].FinishReason; got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
	if len(chunk.Attempts) != 1 {
		t.Fatalf("Attempts = %d entries, want 1: %+v", len(chunk.Attempts), chunk.Attempts)
	}
	att := chunk.Attempts[0]
	if att.Outcome != openai.OutcomeSuccess || !att.OK {
		t.Fatalf("attempt = %+v, want Outcome=success OK=true", att)
	}

	reasons := rankReasons(t, rt, "p1")
	if !containsStr(reasons, "last-known-good") {
		t.Fatalf("p1 reasons = %v, want last-known-good after a real successful completion", reasons)
	}
}

func TestStreamingErrorAfterCommitDoesNotRecordSuccess(t *testing.T) {
	s, rt := newStreamTestServer(t, []provider.StreamEvent{
		{Delta: "hello"},
		{Err: errors.New("upstream exploded")},
	})

	rec := postStreamingChat(t, s)
	chunk := terminalChunk(t, rec.Body.String())

	if got := *chunk.Choices[0].FinishReason; got != "error" {
		t.Fatalf("finish_reason = %q, want error", got)
	}
	if len(chunk.Attempts) != 1 {
		t.Fatalf("Attempts = %d entries, want 1: %+v", len(chunk.Attempts), chunk.Attempts)
	}
	att := chunk.Attempts[0]
	if att.Outcome != openai.OutcomeError || att.OK {
		t.Fatalf("attempt = %+v, want Outcome=error OK=false", att)
	}
	if !strings.Contains(att.Reason, "upstream exploded") {
		t.Fatalf("attempt.Reason = %q, want it to mention the upstream error", att.Reason)
	}

	reasons := rankReasons(t, rt, "p1")
	if containsStr(reasons, "last-known-good") {
		t.Fatalf("p1 reasons = %v, must not be last-known-good after a stream that errored after commit", reasons)
	}
}

func TestStreamingAbnormalCloseWithoutDoneIsAnError(t *testing.T) {
	s, rt := newStreamTestServer(t, []provider.StreamEvent{
		{Delta: "hello"},
		// No Done, no Err — the channel just closes, simulating a dropped
		// connection or an upstream that hangs up mid-answer.
	})

	rec := postStreamingChat(t, s)
	chunk := terminalChunk(t, rec.Body.String())

	if got := *chunk.Choices[0].FinishReason; got != "error" {
		t.Fatalf("finish_reason = %q, want error for an abnormal close", got)
	}
	if len(chunk.Attempts) != 1 {
		t.Fatalf("Attempts = %d entries, want 1: %+v", len(chunk.Attempts), chunk.Attempts)
	}
	att := chunk.Attempts[0]
	if att.Outcome != openai.OutcomeError || att.OK {
		t.Fatalf("attempt = %+v, want Outcome=error OK=false", att)
	}
	if !strings.Contains(att.Reason, "terminal completion") {
		t.Fatalf("attempt.Reason = %q, want it to describe the missing terminal event", att.Reason)
	}

	reasons := rankReasons(t, rt, "p1")
	if containsStr(reasons, "last-known-good") {
		t.Fatalf("p1 reasons = %v, must not be last-known-good after an abnormal close", reasons)
	}
}

func TestStreamingToolCallsCompletionStillSucceeds(t *testing.T) {
	s, rt := newStreamTestServer(t, []provider.StreamEvent{
		{Done: true, ToolCalls: []openai.ToolCall{{ID: "1", Type: "function", Function: openai.ToolCallFunction{Name: "foo", Arguments: "{}"}}}},
	})

	rec := postStreamingChat(t, s)
	chunk := terminalChunk(t, rec.Body.String())

	if got := *chunk.Choices[0].FinishReason; got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got)
	}
	att := chunk.Attempts[0]
	if att.Outcome != openai.OutcomeSuccess || !att.OK {
		t.Fatalf("attempt = %+v, want Outcome=success OK=true", att)
	}

	reasons := rankReasons(t, rt, "p1")
	if !containsStr(reasons, "last-known-good") {
		t.Fatalf("p1 reasons = %v, want last-known-good after a real successful tool-calls completion", reasons)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
