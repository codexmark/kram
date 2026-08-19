package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codexmark/kram/internal/openai"
	"github.com/codexmark/kram/internal/provider"
	"github.com/codexmark/kram/internal/router"
)

// classifyTransportError buckets a genuine transport/HTTP-level failure
// (the request never reached a real answer at all) via openai.Classify,
// extracting the real upstream status code the same way errorAttempt
// does. Never call this for a ResponseGate/StreamGate rejection or a
// BoundedPeek timeout — those aren't transport failures; see
// markRejection.
func classifyTransportError(err error) openai.FailureClass {
	var httpErr *provider.HTTPError
	status := 0
	if errors.As(err, &httpErr) {
		status = httpErr.StatusCode
	}
	return openai.Classify(status, err)
}

// handleChatCompletions is the attempt executor: it asks the router for a
// ranked candidate list (routing strategy), calls each one in order
// (attempt execution), and gates whatever comes back before accepting it
// (response quality) — three concepts the router package keeps separate
// on purpose (see its package doc). A candidate that errors, that a
// ResponseGate rejects, or whose stream never produces a meaningful
// signal (see router.BoundedPeek) all fall through to the next ranked
// candidate the same way; only the eventual winner is reported to the
// router via RecordOutcome, so sticky/LKGP state always reflects who
// really served the request.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req openai.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages must not be empty")
		return
	}

	comboID, err := s.router.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ranked, routeCtx, err := s.router.Rank(comboID, req, r.Header.Get(openai.RunIDHeader))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	gate := s.router.ResponseGateFor(comboID)
	strategyName := s.router.StrategyName(comboID)
	ranking := router.ToRankedProviderInfo(ranked)

	ctx := r.Context()
	var lastErr error
	var trail []openai.AttemptInfo // real per-request fallback trail, streamed back to clients that ask for it

	for i, rc := range ranked {
		p := rc.Provider.Provider
		attemptNum := i + 1
		score := rc.Score

		s.telemetry.RecordAttempt(p.ID())
		attemptStart := time.Now()
		events, err := p.ChatCompletion(ctx, req)
		if err != nil {
			elapsed := time.Since(attemptStart).Milliseconds()
			s.markFailure(p.ID(), err)
			s.telemetry.RecordLatency(p.ID(), elapsed)
			lastErr = err
			trail = append(trail, errorAttempt(p.ID(), elapsed, attemptNum, score, err))
			continue
		}

		if req.Stream {
			peek := router.BoundedPeek(ctx, events)
			elapsed := time.Since(attemptStart).Milliseconds()
			if !peek.Committed {
				lastErr = fmt.Errorf("%s: %s", p.ID(), peek.Reason)
				s.markRejection(p.ID(), peek.Reason)
				s.telemetry.RecordLatency(p.ID(), elapsed)
				trail = append(trail, openai.AttemptInfo{
					Provider: p.ID(), OK: false, LatencyMS: elapsed,
					Outcome: openai.OutcomeRejected, Reason: peek.Reason, Attempt: attemptNum, Score: &score,
					Class: openai.ClassContentRejected,
				})
				continue
			}
			// Committed: headers are about to be written, no more fallback
			// is possible for this request after this point. BoundedPeek
			// only proved a meaningful first signal, not that the request
			// will actually finish successfully, so this attempt's real
			// outcome — and whatever the router does with it (breaker,
			// Sticky, LKGP) — is deliberately not decided here.
			// streamResponse finalizes it once it actually knows how the
			// stream ended; see its doc comment.
			s.telemetry.RecordLatency(p.ID(), elapsed)
			pending := openai.AttemptInfo{Provider: p.ID(), LatencyMS: elapsed, Attempt: attemptNum, Score: &score}
			s.streamResponse(w, comboID, routeCtx, pending, req.Model, peek.Buffered, events, trail, ranking, strategyName)
			return
		}

		content, toolCalls, usage, sawTerminal, err := drainToBuffer(events)
		elapsed := time.Since(attemptStart).Milliseconds()
		if err != nil {
			lastErr = err
			s.markFailure(p.ID(), err)
			s.telemetry.RecordLatency(p.ID(), elapsed)
			trail = append(trail, errorAttempt(p.ID(), elapsed, attemptNum, score, err))
			continue
		}

		outcome := gate.Evaluate(content, toolCalls, sawTerminal)
		if !outcome.Accepted {
			// A rejection here is not a transport/HTTP failure — the
			// provider answered, the ResponseGate just decided the answer
			// wasn't good enough. It still counts as an operational
			// failure for breaker/telemetry purposes (a provider that
			// chronically returns empty or truncated content is not
			// healthy, even though every individual HTTP call "succeeded"),
			// but AttemptInfo.Outcome keeps the distinction visible: this
			// is OutcomeRejected, never OutcomeError.
			lastErr = fmt.Errorf("%s: %s", p.ID(), outcome.Reason)
			s.markRejection(p.ID(), outcome.Reason)
			s.telemetry.RecordLatency(p.ID(), elapsed)
			trail = append(trail, openai.AttemptInfo{
				Provider: p.ID(), OK: false, LatencyMS: elapsed,
				Outcome: openai.OutcomeRejected, Reason: outcome.Reason, Attempt: attemptNum, Score: &score,
				Class: openai.ClassContentRejected,
			})
			continue
		}

		s.breakers.ReportSuccess(p.ID())
		s.telemetry.RecordLatency(p.ID(), elapsed)
		if usage != nil {
			s.telemetry.RecordUsage(p.ID(), usage.PromptTokens, usage.CompletionTokens)
		}
		trail = append(trail, openai.AttemptInfo{
			Provider: p.ID(), OK: true, LatencyMS: elapsed,
			Outcome: openai.OutcomeSuccess, Attempt: attemptNum, Score: &score,
		})
		s.router.RecordOutcome(comboID, routeCtx, p.ID(), true)
		writeBufferedResponse(w, req.Model, p.ID(), content, toolCalls, usage, trail, ranking, strategyName)
		return
	}

	s.router.RecordOutcome(comboID, routeCtx, "", false)
	writeGatewayError(w, comboID, trail, lastErr)
}

// writeGatewayError is the terminal response when every ranked candidate
// in a combo failed — the structured counterpart to a flat message
// string. Retryable/RetryAfterMS/Cause reflect the *last* attempt in
// trail (the one that actually ended the request), a deliberate
// simplification over trying to merge every attempt's class into one
// verdict: a combo whose final candidate failed non-retryably isn't
// worth retrying even if an earlier candidate hit a transient error.
func writeGatewayError(w http.ResponseWriter, comboID string, trail []openai.AttemptInfo, lastErr error) {
	body := openai.ErrorBody{
		Message:  fmt.Sprintf("all providers in combo %q failed, last error: %v", comboID, lastErr),
		Type:     "kram_gateway_error",
		Combo:    comboID,
		Attempts: trail,
	}
	if len(trail) > 0 {
		last := trail[len(trail)-1]
		body.Cause = last.Class
		body.Retryable = last.Class.Retryable()
	}
	var httpErr *provider.HTTPError
	if errors.As(lastErr, &httpErr) && httpErr.RetryAfter > 0 {
		body.RetryAfterMS = httpErr.RetryAfter.Milliseconds()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(openai.ErrorResponse{Error: body})
}

// errorAttempt builds a trail entry for a transport/HTTP-level failure,
// extracting the real upstream status code when the provider adapter
// returned a *provider.HTTPError (see provider.go) instead of a generic
// wrapped error.
func errorAttempt(providerID string, elapsedMS int64, attempt int, score float64, err error) openai.AttemptInfo {
	info := openai.AttemptInfo{
		Provider: providerID, OK: false, LatencyMS: elapsedMS,
		Outcome: openai.OutcomeError, Reason: err.Error(), Attempt: attempt, Score: &score,
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) {
		info.HTTPStatus = httpErr.StatusCode
	}
	info.Class = openai.Classify(info.HTTPStatus, err)
	return info
}

// markFailure reports a genuine transport/HTTP-level failure — the
// request never reached a real, gate-evaluated answer. Whether it counts
// against the circuit breaker depends on its FailureClass: a 400 caused
// by Kram's own malformed request (see DECISIONS.md's Gemini role-name
// bug) must not poison an otherwise-healthy provider's breaker the same
// way a genuine 5xx/network failure does.
func (s *Server) markFailure(id string, err error) {
	class := classifyTransportError(err)
	if class.CountsAgainstBreaker() {
		s.breakers.ReportFailure(id)
	}
	s.telemetry.RecordFailure(id)
	s.logger.Warn("provider attempt failed", "provider", id, "class", class, "error", err)
}

// markRejection reports a content-level rejection — a ResponseGate/
// StreamGate decision, or a BoundedPeek timeout — never a transport
// failure, so it's never routed through Classify (see
// openai.ClassContentRejected's doc comment). Always counts against the
// breaker: a provider that chronically returns short, empty, or
// never-quite-arriving content is not healthy, even though every
// individual HTTP call "succeeded".
func (s *Server) markRejection(id, reason string) {
	s.breakers.ReportFailure(id)
	s.telemetry.RecordFailure(id)
	s.logger.Warn("provider attempt rejected", "provider", id, "class", openai.ClassContentRejected, "reason", reason)
}

// drainToBuffer fully consumes a provider's stream and concatenates its
// deltas and any tool calls. Used for non-streaming requests, where
// nothing is written to the client until we know the whole response
// succeeded — so a failure (or a ResponseGate rejection) here can still
// fall back to the next provider. sawTerminal reports whether the
// provider ever sent a Done event before its channel closed — a stream
// that's cut off mid-flight without one is what
// config.ResponseGateConfig.RequireTerminal catches.
func drainToBuffer(events <-chan provider.StreamEvent) (content string, toolCalls []openai.ToolCall, usage *openai.Usage, sawTerminal bool, err error) {
	var b strings.Builder
	for evt := range events {
		if evt.Err != nil {
			return "", nil, nil, false, evt.Err
		}
		b.WriteString(evt.Delta)
		if evt.Usage != nil {
			usage = evt.Usage
		}
		if evt.Done {
			toolCalls = evt.ToolCalls
			sawTerminal = true
			break
		}
	}
	return b.String(), toolCalls, usage, sawTerminal, nil
}

func writeBufferedResponse(w http.ResponseWriter, model, providerID, content string, toolCalls []openai.ToolCall, usage *openai.Usage, trail []openai.AttemptInfo, ranking []openai.RankedProviderInfo, strategyName string) {
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	resp := openai.ChatCompletionResponse{
		ID:      newID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openai.ChatCompletionChoice{
			{Index: 0, Message: openai.ChatMessage{Role: "assistant", Content: content, ToolCalls: toolCalls}, FinishReason: finish},
		},
		Provider: providerID,
		Attempts: trail,
		Ranking:  ranking,
		Strategy: strategyName,
	}
	if usage != nil {
		resp.Usage = *usage
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// streamResponse writes SSE chunks to w as events arrive. buffered is
// whatever router.BoundedPeek already consumed deciding to commit — it's
// replayed first, in order, before continuing to read fresh events from
// rest, so the client sees exactly the same stream it would have seen
// without the peek ever happening. Once this is called, the response is
// committed: headers are sent immediately, so no further fallback is
// possible if a later event carries an error.
//
// pending is this attempt's trail entry with everything already known at
// commit time (provider, latency, attempt number, score) but no Outcome
// yet — BoundedPeek proved only a meaningful first signal, not that the
// request will actually finish successfully. streamResponse is the one
// place that decides and reports the real terminal state: a valid Done
// marks success (breaker, RecordOutcome — so Sticky/LKGP only ever
// reflect a request that actually completed); an explicit evt.Err, or
// the upstream channel closing without ever sending Done, both mark
// failure and produce an explicit error terminal chunk instead of a bare
// [DONE] that would otherwise make an abnormal close look identical to a
// normal finish.
func (s *Server) streamResponse(w http.ResponseWriter, comboID string, routeCtx router.RouteContext, pending openai.AttemptInfo, model string, buffered []provider.StreamEvent, rest <-chan provider.StreamEvent, priorTrail []openai.AttemptInfo, ranking []openai.RankedProviderInfo, strategyName string) {
	flusher, _ := w.(http.Flusher)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := newID()
	created := time.Now().Unix()

	write := func(chunk openai.ChatCompletionChunk) {
		b, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeDelta := func(delta, role string) {
		write(openai.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChatCompletionChunkChoice{{
				Index: 0, Delta: openai.ChatCompletionChunkDelta{Role: role, Content: delta},
			}},
		})
	}

	// finalize is the single place this attempt's terminal state becomes
	// truthful and final — the router only ever hears about success here,
	// once it's real, so the client's trail and the router's own Sticky/
	// LKGP state can never disagree about who actually won.
	finalize := func(outcome openai.AttemptOutcome, ok bool, reason string, class openai.FailureClass) []openai.AttemptInfo {
		pending.Outcome = outcome
		pending.OK = ok
		pending.Reason = reason
		pending.Class = class
		trail := append(append([]openai.AttemptInfo{}, priorTrail...), pending)
		if ok {
			s.router.RecordOutcome(comboID, routeCtx, pending.Provider, true)
		}
		return trail
	}

	writeFinal := func(finish string, toolCalls []openai.ToolCall, usage *openai.Usage, trail []openai.AttemptInfo) {
		chunk := openai.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChatCompletionChunkChoice{{
				Index:        0,
				Delta:        openai.ChatCompletionChunkDelta{ToolCalls: toolCalls},
				FinishReason: &finish,
			}},
			Provider: pending.Provider,
			Attempts: trail,
			Ranking:  ranking,
			Strategy: strategyName,
			Usage:    usage,
		}
		write(chunk)
	}

	writeDelta("", "assistant")

	sawTerminal := false
	handle := func(evt provider.StreamEvent) (keepGoing bool) {
		if evt.Err != nil {
			sawTerminal = true
			s.markFailure(pending.Provider, evt.Err)
			writeFinal("error", nil, nil, finalize(openai.OutcomeError, false, evt.Err.Error(), classifyTransportError(evt.Err)))
			return false
		}
		if evt.Delta != "" {
			writeDelta(evt.Delta, "")
		}
		if evt.Done {
			sawTerminal = true
			s.breakers.ReportSuccess(pending.Provider)
			if evt.Usage != nil {
				s.telemetry.RecordUsage(pending.Provider, evt.Usage.PromptTokens, evt.Usage.CompletionTokens)
			}
			finish := "stop"
			if len(evt.ToolCalls) > 0 {
				finish = "tool_calls"
			}
			writeFinal(finish, evt.ToolCalls, evt.Usage, finalize(openai.OutcomeSuccess, true, "", ""))
			return false
		}
		return true
	}

	keepGoing := true
	for _, evt := range buffered {
		if !handle(evt) {
			keepGoing = false
			break
		}
	}
	if keepGoing {
		for evt := range rest {
			if !handle(evt) {
				break
			}
		}
	}

	if !sawTerminal {
		// The upstream channel closed on its own without ever sending a
		// terminal Done or an explicit error — an abnormal/truncated
		// stream. Without this, the code below would simply write a bare
		// [DONE], indistinguishable on the wire from a normal finish —
		// silently turning a truncated answer into an apparent success.
		err := fmt.Errorf("%s: stream closed without a terminal completion", pending.Provider)
		s.markFailure(pending.Provider, err)
		writeFinal("error", nil, nil, finalize(openai.OutcomeError, false, err.Error(), classifyTransportError(err)))
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func newID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return "chatcmpl-" + hex.EncodeToString(buf)
}
