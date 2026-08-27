package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codexmark/kram/internal/openai"
)

// TestStreamWatchdogAllowsStreamLongerThanTimeout is the regression test
// for the mid-answer kill: the old http.Client whole-call timeout cut off
// any generation whose total time exceeded it ("stream read: context
// deadline exceeded ... while reading body"). A stream whose *total*
// duration far exceeds the configured timeout, but which keeps delivering
// chunks faster than it, must now complete.
func TestStreamWatchdogAllowsStreamLongerThanTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n")
			fl.Flush()
			time.Sleep(40 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	p := NewOpenAICompatible("p", srv.URL, "k", "", nil, capabilities{})
	p.timeout = 150 * time.Millisecond // total stream time (~320ms) far exceeds this; per-chunk gaps never do

	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var content string
	var sawDone bool
	for e := range events {
		if e.Err != nil {
			t.Fatalf("flowing stream longer than the timeout errored: %v", e.Err)
		}
		content += e.Delta
		if e.Done {
			sawDone = true
		}
	}
	if !sawDone || content != strings.Repeat("x", 8) {
		t.Fatalf("stream did not complete: done=%v content=%q", sawDone, content)
	}
}

// TestStreamWatchdogKillsQuietStream: an upstream that stops sending
// entirely must still die at the threshold — the watchdog keeps the old
// timeout's dead-upstream protection, and labels the failure as an idle
// timeout instead of a generic context cancellation.
func TestStreamWatchdogKillsQuietStream(t *testing.T) {
	streamDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"start\"},\"finish_reason\":null}]}\n\n")
		fl.Flush()
		<-streamDone // go quiet forever (until the test ends)
	}))
	defer srv.Close()
	defer close(streamDone)

	p := NewOpenAICompatible("p", srv.URL, "k", "", nil, capabilities{})
	p.timeout = 60 * time.Millisecond

	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for e := range events {
		if e.Err != nil {
			streamErr = e.Err
		}
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "idle timeout") {
		t.Fatalf("quiet stream error = %v, want an idle-timeout-labeled failure", streamErr)
	}
}

// TestStreamWatchdogBoundsHeadersPhase: a server that never even answers
// with headers fails at the same threshold, before any body exists.
func TestStreamWatchdogBoundsHeadersPhase(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // never write headers
	}))
	defer srv.Close()
	defer close(blocked)

	p := NewOpenAICompatible("p", srv.URL, "k", "", nil, capabilities{})
	p.timeout = 60 * time.Millisecond

	_, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("headers-phase hang error = %v, want an idle-timeout-labeled failure", err)
	}
}
