package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codexmark/kram-gateway/internal/openai"
	"github.com/codexmark/kram-gateway/internal/provider"
)

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

	candidates, err := s.router.Attempts(comboID, affinityKey(req))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	ctx := r.Context()
	var lastErr error
	var trail []openai.AttemptInfo // real per-request fallback trail, streamed back to clients that ask for it

	for _, p := range candidates {
		s.telemetry.RecordAttempt(p.ID())
		attemptStart := time.Now()
		events, err := p.ChatCompletion(ctx, req)
		if err != nil {
			elapsed := time.Since(attemptStart).Milliseconds()
			s.markFailure(p.ID(), err)
			s.telemetry.RecordLatency(p.ID(), elapsed)
			lastErr = err
			trail = append(trail, openai.AttemptInfo{Provider: p.ID(), OK: false, LatencyMS: elapsed})
			continue
		}

		if req.Stream {
			first, ok := <-events
			if !ok {
				elapsed := time.Since(attemptStart).Milliseconds()
				lastErr = fmt.Errorf("%s: closed stream with no data", p.ID())
				s.markFailure(p.ID(), lastErr)
				s.telemetry.RecordLatency(p.ID(), elapsed)
				trail = append(trail, openai.AttemptInfo{Provider: p.ID(), OK: false, LatencyMS: elapsed})
				continue
			}
			if first.Err != nil {
				elapsed := time.Since(attemptStart).Milliseconds()
				lastErr = first.Err
				s.markFailure(p.ID(), lastErr)
				s.telemetry.RecordLatency(p.ID(), elapsed)
				trail = append(trail, openai.AttemptInfo{Provider: p.ID(), OK: false, LatencyMS: elapsed})
				continue
			}
			// Committed: headers are about to be written, no more fallback
			// is possible for this request after this point.
			commitElapsed := time.Since(attemptStart).Milliseconds()
			s.telemetry.RecordLatency(p.ID(), commitElapsed)
			trail = append(trail, openai.AttemptInfo{Provider: p.ID(), OK: true, LatencyMS: commitElapsed})
			s.streamResponse(w, p.ID(), req.Model, first, events, trail)
			return
		}

		content, toolCalls, usage, err := drainToBuffer(events)
		elapsed := time.Since(attemptStart).Milliseconds()
		if err != nil {
			lastErr = err
			s.markFailure(p.ID(), err)
			s.telemetry.RecordLatency(p.ID(), elapsed)
			trail = append(trail, openai.AttemptInfo{Provider: p.ID(), OK: false, LatencyMS: elapsed})
			continue
		}
		s.breakers.ReportSuccess(p.ID())
		s.telemetry.RecordLatency(p.ID(), elapsed)
		if usage != nil {
			s.telemetry.RecordUsage(p.ID(), usage.PromptTokens, usage.CompletionTokens)
		}
		trail = append(trail, openai.AttemptInfo{Provider: p.ID(), OK: true, LatencyMS: elapsed})
		writeBufferedResponse(w, req.Model, p.ID(), content, toolCalls, usage, trail)
		return
	}

	writeError(w, http.StatusBadGateway, fmt.Sprintf("all providers in combo %q failed, last error: %v", comboID, lastErr))
}

func (s *Server) markFailure(id string, err error) {
	s.breakers.ReportFailure(id)
	s.telemetry.RecordFailure(id)
	s.logger.Warn("provider attempt failed", "provider", id, "error", err)
}

// drainToBuffer fully consumes a provider's stream and concatenates its
// deltas and any tool calls. Used for non-streaming requests, where
// nothing is written to the client until we know the whole response
// succeeded — so a failure here can still fall back to the next provider.
func drainToBuffer(events <-chan provider.StreamEvent) (string, []openai.ToolCall, *openai.Usage, error) {
	var content strings.Builder
	var toolCalls []openai.ToolCall
	var usage *openai.Usage
	for evt := range events {
		if evt.Err != nil {
			return "", nil, nil, evt.Err
		}
		content.WriteString(evt.Delta)
		if evt.Usage != nil {
			usage = evt.Usage
		}
		if evt.Done {
			toolCalls = evt.ToolCalls
			break
		}
	}
	return content.String(), toolCalls, usage, nil
}

func writeBufferedResponse(w http.ResponseWriter, model, providerID, content string, toolCalls []openai.ToolCall, usage *openai.Usage, trail []openai.AttemptInfo) {
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
	}
	if usage != nil {
		resp.Usage = *usage
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// streamResponse writes SSE chunks to w as events arrive. Once called, the
// response is committed: headers are sent immediately, so no further
// fallback is possible if a later event carries an error. The terminal
// chunk carries provider/attempts/usage/tool_calls (kram-gateway
// extensions) so a caller can run entirely off the streaming path — the
// daemon's agent loop does exactly that, rather than needing a separate
// non-streaming request.
func (s *Server) streamResponse(w http.ResponseWriter, providerID, model string, first provider.StreamEvent, rest <-chan provider.StreamEvent, trail []openai.AttemptInfo) {
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

	writeFinal := func(finish string, toolCalls []openai.ToolCall, usage *openai.Usage) {
		chunk := openai.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChatCompletionChunkChoice{{
				Index:        0,
				Delta:        openai.ChatCompletionChunkDelta{ToolCalls: toolCalls},
				FinishReason: &finish,
			}},
			Provider: providerID,
			Attempts: trail,
			Usage:    usage,
		}
		write(chunk)
	}

	writeDelta("", "assistant")

	handle := func(evt provider.StreamEvent) (keepGoing bool) {
		if evt.Err != nil {
			s.markFailure(providerID, evt.Err)
			writeFinal("error", nil, nil)
			return false
		}
		if evt.Delta != "" {
			writeDelta(evt.Delta, "")
		}
		if evt.Done {
			s.breakers.ReportSuccess(providerID)
			if evt.Usage != nil {
				s.telemetry.RecordUsage(providerID, evt.Usage.PromptTokens, evt.Usage.CompletionTokens)
			}
			finish := "stop"
			if len(evt.ToolCalls) > 0 {
				finish = "tool_calls"
			}
			writeFinal(finish, evt.ToolCalls, evt.Usage)
			return false
		}
		return true
	}

	if handle(first) {
		for evt := range rest {
			if !handle(evt) {
				break
			}
		}
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

// affinityKey identifies a request's stable prompt prefix, for combos
// using the prefix-affinity strategy (see router.StrategyPrefixAffinity).
//
// It's built from the leading system messages plus the first user
// message, which is precisely the part that does not change across an
// agent turn's tool round-trips — the growing tail of tool calls and
// results is deliberately excluded, since including it would produce a
// different key on every round-trip and defeat the entire purpose.
func affinityKey(req openai.ChatCompletionRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" {
			b.WriteString(m.Content)
			continue
		}
		if m.Role == "user" {
			b.WriteString(m.Content)
			break
		}
	}
	return b.String()
}
