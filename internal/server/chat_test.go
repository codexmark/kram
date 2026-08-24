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
	"time"

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

// breakerFailureThreshold mirrors internal/breaker's unexported
// failureThreshold (3) — duplicated here since it's not exported; keep
// in sync if that constant ever changes.
const breakerFailureThreshold = 3

// newBufferedTestServer wires a single-provider "default" combo (no
// fallback candidate, so a failing p1 fails the whole request cleanly —
// exactly what the breaker-poisoning tests below need to isolate).
func newBufferedTestServer(t *testing.T, p1Events []provider.StreamEvent) (*Server, *breaker.Registry) {
	t.Helper()
	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{ID: "p1", Kind: "fake"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"p1"}}},
		DefaultCombo: "default",
	}
	providers := map[string]provider.Provider{"p1": scriptedProvider{id: "p1", events: p1Events}}
	breakers := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(cfg, providers, breakers, tel)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, providers, rt, breakers, tel, logger), breakers
}

const bufferedTestBody = `{"model":"default","messages":[{"role":"user","content":"hi"}]}`

func postBufferedChat(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(bufferedTestBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestKramCausedBadRequestDoesNotPoisonBreaker is the regression test for
// the confirmed bug: a 400 caused by Kram's own malformed request (the
// real historical example being the Gemini role:"function" bug — see
// DECISIONS.md) used to trip a provider's circuit breaker exactly like a
// genuine upstream instability would, taking a perfectly healthy
// provider out of rotation for a bug that was never its fault.
func TestKramCausedBadRequestDoesNotPoisonBreaker(t *testing.T) {
	s, breakers := newBufferedTestServer(t, []provider.StreamEvent{
		{Err: &provider.HTTPError{Provider: "p1", StatusCode: 400, Status: "400 Bad Request"}},
	})

	for i := 0; i < breakerFailureThreshold+1; i++ {
		postBufferedChat(t, s)
	}

	if breakers.IsOpen("p1") {
		t.Error("a provider that only ever returned 400s (Kram's own malformed request) must not have its breaker tripped")
	}
}

// TestGenuineServerErrorStillPoisonsBreaker confirms the fix didn't
// overcorrect: a real 5xx must still trip the breaker exactly as before.
func TestGenuineServerErrorStillPoisonsBreaker(t *testing.T) {
	s, breakers := newBufferedTestServer(t, []provider.StreamEvent{
		{Err: &provider.HTTPError{Provider: "p1", StatusCode: 500, Status: "500 Internal Server Error"}},
	})

	for i := 0; i < breakerFailureThreshold; i++ {
		postBufferedChat(t, s)
	}

	if !breakers.IsOpen("p1") {
		t.Error("a provider returning genuine 500s should trip the breaker after breakerFailureThreshold consecutive failures")
	}
}

// TestResponseGateRejectionStillPoisonsBreaker confirms markRejection
// kept the pre-existing behavior for content-level rejections — these
// are deliberately never routed through Classify (see
// openai.ClassContentRejected's doc comment), so they must keep counting
// against the breaker unconditionally, same as before this change.
func TestResponseGateRejectionStillPoisonsBreaker(t *testing.T) {
	// No Done event at all -> drainToBuffer returns sawTerminal=false,
	// nil err; ResponseGate's zero-value config only rejects on
	// RequireTerminal, so wire that on for this test via a custom combo.
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{ID: "p1", Kind: "fake"}},
		Combos: []config.ComboConfig{{
			ID: "default", Strategy: "priority", Providers: []string{"p1"},
			Response: config.ResponseGateConfig{RequireTerminal: true},
		}},
		DefaultCombo: "default",
	}
	providers := map[string]provider.Provider{"p1": scriptedProvider{id: "p1", events: nil}} // closes with no Done
	breakers := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(cfg, providers, breakers, tel)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(cfg, providers, rt, breakers, tel, logger)

	for i := 0; i < breakerFailureThreshold; i++ {
		postBufferedChat(t, s)
	}

	if !breakers.IsOpen("p1") {
		t.Error("a ResponseGate rejection (RequireTerminal, no Done ever seen) should still poison the breaker after breakerFailureThreshold rejections")
	}
}

func TestBufferedFallbackUsageIncludesRejectedCandidate(t *testing.T) {
	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{ID: "p1", Kind: "fake"}, {ID: "p2", Kind: "fake"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"p1", "p2"}, Response: config.ResponseGateConfig{RequireTerminal: true}}},
		DefaultCombo: "default",
	}
	p1Usage := &openai.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
	p2Usage := &openai.Usage{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55}
	providers := map[string]provider.Provider{
		"p1": scriptedProvider{id: "p1", events: []provider.StreamEvent{{Delta: "rejected", Usage: p1Usage}}},
		"p2": scriptedProvider{id: "p2", events: []provider.StreamEvent{{Delta: "accepted", Done: true, Usage: p2Usage}}},
	}
	breakers := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(cfg, providers, breakers, tel)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, providers, rt, breakers, tel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := postBufferedChat(t, s)
	var response openai.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Provider != "p2" || response.Usage.TotalTokens != 165 || len(response.Attempts) != 2 || response.Attempts[0].Usage == nil {
		t.Fatalf("response = %+v", response)
	}
}

// TestAllProvidersFailedReturnsStructuredGatewayError is the regression
// test for the old flat-string-only error: a caller must be able to
// decode Combo/Retryable/Cause/Attempts, not just parse an English
// sentence, to make a real retry decision.
func TestAllProvidersFailedReturnsStructuredGatewayError(t *testing.T) {
	s, _ := newBufferedTestServer(t, []provider.StreamEvent{
		{Err: &provider.HTTPError{Provider: "p1", StatusCode: 500, Status: "500 Internal Server Error"}},
	})

	rec := postBufferedChat(t, s)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	var errResp openai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if errResp.Error.Combo != "default" {
		t.Errorf("Combo = %q, want %q", errResp.Error.Combo, "default")
	}
	if !errResp.Error.Retryable {
		t.Error("Retryable = false, want true for a 500 (ClassServerError)")
	}
	if errResp.Error.Cause != openai.ClassServerError {
		t.Errorf("Cause = %q, want %q", errResp.Error.Cause, openai.ClassServerError)
	}
	if len(errResp.Error.Attempts) != 1 {
		t.Fatalf("Attempts = %d entries, want 1: %+v", len(errResp.Error.Attempts), errResp.Error.Attempts)
	}
	if errResp.Error.Attempts[0].HTTPStatus != 500 {
		t.Errorf("Attempts[0].HTTPStatus = %d, want 500", errResp.Error.Attempts[0].HTTPStatus)
	}
}

// TestAllProvidersFailedWithInvalidRequestIsNotRetryable confirms the
// "last attempt's class wins" rule produces the right verdict for a
// non-retryable terminal failure.
func TestAllProvidersFailedWithInvalidRequestIsNotRetryable(t *testing.T) {
	s, _ := newBufferedTestServer(t, []provider.StreamEvent{
		{Err: &provider.HTTPError{Provider: "p1", StatusCode: 400, Status: "400 Bad Request"}},
	})

	rec := postBufferedChat(t, s)
	var errResp openai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if errResp.Error.Retryable {
		t.Error("Retryable = true, want false for a 400 (ClassInvalidRequest)")
	}
	if errResp.Error.Cause != openai.ClassInvalidRequest {
		t.Errorf("Cause = %q, want %q", errResp.Error.Cause, openai.ClassInvalidRequest)
	}
}

// TestAllProvidersFailedIsRetryableIfAnyAttemptWas is the regression
// test for the confirmed bug: Retryable used to reflect only the *last*
// attempt's class, which is wrong whenever ranking order happens to put
// a permanently-broken candidate (a 404 for a retired model, say) after
// a merely transient one (429/503) — the round as a whole is still
// worth retrying, since which candidate was tried last is an accident
// of ranking order, not evidence every candidate is hopeless.
func TestAllProvidersFailedIsRetryableIfAnyAttemptWas(t *testing.T) {
	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{ID: "p1", Kind: "fake"}, {ID: "p2", Kind: "fake"}, {ID: "p3", Kind: "fake"}},
		Combos:       []config.ComboConfig{{ID: "default", Strategy: "priority", Providers: []string{"p1", "p2", "p3"}}},
		DefaultCombo: "default",
	}
	providers := map[string]provider.Provider{
		"p1": scriptedProvider{id: "p1", events: []provider.StreamEvent{{Err: &provider.HTTPError{Provider: "p1", StatusCode: 429, Status: "429 Too Many Requests"}}}},
		"p2": scriptedProvider{id: "p2", events: []provider.StreamEvent{{Err: &provider.HTTPError{Provider: "p2", StatusCode: 503, Status: "503 Service Unavailable"}}}},
		"p3": scriptedProvider{id: "p3", events: []provider.StreamEvent{{Err: &provider.HTTPError{Provider: "p3", StatusCode: 404, Status: "404 Not Found"}}}},
	}
	breakers := breaker.NewRegistry()
	tel := telemetry.New()
	rt, err := router.New(cfg, providers, breakers, tel)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(cfg, providers, rt, breakers, tel, logger)

	rec := postBufferedChat(t, s)
	var errResp openai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if len(errResp.Error.Attempts) != 3 {
		t.Fatalf("expected 3 attempts in the trail, got %d: %+v", len(errResp.Error.Attempts), errResp.Error.Attempts)
	}
	if !errResp.Error.Retryable {
		t.Error("Retryable = false, want true — p1 (429) was retryable even though the last candidate (p3, 404) wasn't")
	}
	// Cause still reflects the last attempt — still meaningful as "what
	// ultimately ended this request", just no longer conflated with
	// Retryable.
	if errResp.Error.Cause != openai.ClassInvalidRequest {
		t.Errorf("Cause = %q, want %q (p3's class, the last attempt)", errResp.Error.Cause, openai.ClassInvalidRequest)
	}
}

// TestAllProvidersFailedCarriesRetryAfter confirms a real Retry-After
// from the upstream survives all the way to the wire response.
func TestAllProvidersFailedCarriesRetryAfter(t *testing.T) {
	s, _ := newBufferedTestServer(t, []provider.StreamEvent{
		{Err: &provider.HTTPError{Provider: "p1", StatusCode: 429, Status: "429 Too Many Requests", RetryAfter: 7 * time.Second}},
	})

	rec := postBufferedChat(t, s)
	var errResp openai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if errResp.Error.RetryAfterMS != 7000 {
		t.Errorf("RetryAfterMS = %d, want 7000", errResp.Error.RetryAfterMS)
	}
}

func TestErrorAttemptSetsFailureClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want openai.FailureClass
	}{
		{"400 bad request", &provider.HTTPError{StatusCode: 400}, openai.ClassInvalidRequest},
		{"429 rate limit", &provider.HTTPError{StatusCode: 429}, openai.ClassRateLimit},
		{"500 server error", &provider.HTTPError{StatusCode: 500}, openai.ClassServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := errorAttempt("p1", 10, 1, 0.5, c.err)
			if info.Class != c.want {
				t.Errorf("errorAttempt(%v).Class = %q, want %q", c.err, info.Class, c.want)
			}
		})
	}
}
