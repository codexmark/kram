package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/codexmark/kram/internal/daemon/agent"
)

// A turn is one agent run decoupled from any single HTTP connection: the
// run holds its own context (canceled only by an explicit interrupt —
// see handleInterrupt), publishes every event frame into a replay buffer,
// and any number of SSE subscribers can attach, detach and reattach while
// it runs. This is what lets a terminal crash at minute 8 of a long task
// leave the work running server-side (#112): closing the stream merely
// unsubscribes; it no longer cancels anything.

const (
	// turnReplayCapBytes bounds the replay buffer. When exceeded, the
	// oldest frames are discarded and a reattaching client gets a notice
	// that earlier output was omitted — the final done frame still
	// carries the complete persisted message, so nothing is lost for
	// good.
	turnReplayCapBytes = 4 << 20
	// turnRetention keeps a finished turn around briefly so a client that
	// disconnected right before completion can still reattach and receive
	// the replay plus the terminal frame instead of finding nothing.
	turnRetention = 60 * time.Second
	// turnSubscriberBuffer is each subscriber's frame-channel capacity. A
	// subscriber slower than this many frames behind has them dropped
	// (never blocking the run itself); the done frame's complete message
	// makes the transcript whole again.
	turnSubscriberBuffer = 4096
)

type turn struct {
	mu          sync.Mutex
	frames      []json.RawMessage
	bufferBytes int
	dropped     bool // replay cap hit; oldest frames discarded
	subs        map[int]chan json.RawMessage
	nextSub     int
	done        bool
	cancel      context.CancelFunc
}

func (t *turn) publish(frame json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.frames = append(t.frames, frame)
	t.bufferBytes += len(frame)
	for t.bufferBytes > turnReplayCapBytes && len(t.frames) > 1 {
		t.bufferBytes -= len(t.frames[0])
		t.frames = t.frames[1:]
		t.dropped = true
	}
	for _, ch := range t.subs {
		select {
		case ch <- frame:
		default: // slow subscriber: drop for them, never stall the run
		}
	}
}

// finish publishes the terminal frame and closes every subscriber
// channel — buffered frames drain first (channel semantics), then the
// closed channel tells each subscriber to write [DONE] and return.
func (t *turn) finish(frame json.RawMessage) {
	t.publish(frame)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
	for id, ch := range t.subs {
		close(ch)
		delete(t.subs, id)
	}
}

// subscribe returns the replay so far and, unless the turn already
// finished, a live channel plus an unsubscribe func. finished==true means
// the replay is complete through the terminal frame.
func (t *turn) subscribe() (replay []json.RawMessage, droppedEarly bool, live <-chan json.RawMessage, unsubscribe func(), finished bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	replay = make([]json.RawMessage, len(t.frames))
	copy(replay, t.frames)
	if t.done {
		return replay, t.dropped, nil, func() {}, true
	}
	ch := make(chan json.RawMessage, turnSubscriberBuffer)
	id := t.nextSub
	t.nextSub++
	t.subs[id] = ch
	return replay, t.dropped, ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, ok := t.subs[id]; ok {
			close(ch)
			delete(t.subs, id)
		}
	}, false
}

type turnRegistry struct {
	mu    sync.Mutex
	turns map[string]*turn
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{turns: make(map[string]*turn)}
}

// start registers a new active turn for the session, or reports the one
// already running — one turn per session, always.
func (r *turnRegistry) start(sessionID string, cancel context.CancelFunc) (*turn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.turns[sessionID]; ok && !existing.isDone() {
		return existing, false
	}
	t := &turn{subs: make(map[int]chan json.RawMessage), cancel: cancel}
	r.turns[sessionID] = t
	return t, true
}

func (t *turn) isDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

func (r *turnRegistry) get(sessionID string) *turn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns[sessionID]
}

// retire schedules a finished turn's removal after turnRetention, so a
// briefly-disconnected client can still reattach for the terminal frame.
func (r *turnRegistry) retire(sessionID string, t *turn) {
	time.AfterFunc(turnRetention, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.turns[sessionID] == t {
			delete(r.turns, sessionID)
		}
	})
}

// interrupt cancels the session's active turn — the explicit replacement
// for the old "closing the stream cancels the run" contract.
func (r *turnRegistry) interrupt(sessionID string) bool {
	r.mu.Lock()
	t := r.turns[sessionID]
	r.mu.Unlock()
	if t == nil || t.isDone() {
		return false
	}
	if t.cancel != nil {
		t.cancel()
	}
	return true
}

// mustFrame marshals a wire event; marshaling a map of primitives cannot
// fail, but stay total anyway.
func mustFrame(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"type": "error", "error": "internal: encoding event"})
	}
	return b
}

// eventFrame converts one agent event into its wire frame. ok=false for
// events that produce no frame.
func eventFrame(evt agent.Event) (json.RawMessage, bool) {
	switch evt.Kind {
	case agent.EventDelta:
		return mustFrame(map[string]any{"type": "delta", "content": evt.Content}), true
	case agent.EventReasoning:
		return mustFrame(map[string]any{"type": "reasoning", "content": evt.Reasoning}), true
	case agent.EventToolStart:
		return mustFrame(map[string]any{"type": "tool_start", "name": evt.ToolName, "args": evt.ToolArgs}), true
	case agent.EventToolResult:
		return mustFrame(map[string]any{
			"type": "tool_result", "name": evt.ToolName, "result": evt.ToolResult,
			"ok": evt.ToolOK, "process_id": evt.ProcessID,
		}), true
	case agent.EventNotice:
		return mustFrame(map[string]any{"type": "notice", "text": evt.Notice}), true
	case agent.EventQuestion:
		return mustFrame(map[string]any{"type": "question", "question_id": evt.QuestionID, "question": evt.Question, "options": evt.Options}), true
	case agent.EventApproval:
		return mustFrame(map[string]any{
			"type": "approval", "approval_id": evt.ApprovalID, "tool": evt.ApprovalTool,
			"subject": evt.ApprovalSubject, "diff": evt.ApprovalDiff,
			"options": []string{"once", "always", "deny"},
		}), true
	case agent.EventRouteStart:
		return mustFrame(map[string]any{"type": "route_start"}), true
	case agent.EventRouteDone:
		return mustFrame(map[string]any{"type": "route_done", "route_call": evt.RouteCall}), true
	case agent.EventHeartbeat:
		// Payload-free on purpose — see EventHeartbeat's doc comment.
		return mustFrame(map[string]any{"type": "heartbeat"}), true
	case agent.EventSegment:
		return mustFrame(map[string]any{"type": "segment", "segment": evt.Segment, "segments": evt.Segments}), true
	default:
		return nil, false
	}
}

// streamTurn subscribes w to the turn: replay first (with an honest
// notice when the replay cap discarded early frames), then live frames
// until the turn finishes ([DONE]) or the client disconnects (plain
// unsubscribe — the run continues).
func (s *Server) streamTurn(w http.ResponseWriter, r *http.Request, t *turn) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeFrame := func(frame json.RawMessage) {
		fmt.Fprintf(w, "data: %s\n\n", frame)
		flusher.Flush()
	}

	replay, droppedEarly, live, unsubscribe, finished := t.subscribe()
	defer unsubscribe()
	if droppedEarly {
		writeFrame(mustFrame(map[string]any{"type": "notice", "text": "reattached mid-turn — earliest output was trimmed from the replay (the final message is complete)"}))
	}
	for _, frame := range replay {
		writeFrame(frame)
	}
	if finished {
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	for {
		select {
		case frame, open := <-live:
			if !open {
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			writeFrame(frame)
		case <-r.Context().Done():
			return // client went away; the turn keeps running
		}
	}
}
