package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sseServer(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func collectDeltas(t *testing.T, deltas <-chan StreamDelta) []StreamDelta {
	t.Helper()
	var out []StreamDelta
	for d := range deltas {
		out = append(out, d)
	}
	return out
}

func TestChatCompletionStreamAssemblesTextDeltas(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"provider":"p1"}`,
	})
	defer srv.Close()

	c := New(srv.URL)
	deltas, err := c.ChatCompletionStream(context.Background(), "default", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := collectDeltas(t, deltas)

	var text string
	var final *StreamDelta
	for i := range got {
		text += got[i].Content
		if got[i].Done {
			final = &got[i]
		}
	}
	if text != "Hello" {
		t.Errorf("assembled text = %q, want %q", text, "Hello")
	}
	if final == nil {
		t.Fatal("expected a final Done delta")
	}
	if final.Provider != "p1" {
		t.Errorf("final.Provider = %q, want %q", final.Provider, "p1")
	}
}

func TestChatCompletionStreamCarriesToolCallsOnFinish(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	})
	defer srv.Close()

	c := New(srv.URL)
	deltas, err := c.ChatCompletionStream(context.Background(), "default", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := collectDeltas(t, deltas)

	if len(got) != 1 || !got[0].Done {
		t.Fatalf("expected exactly one Done delta, got %+v", got)
	}
	if len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "call_1" {
		t.Errorf("ToolCalls = %+v, want one entry for call_1", got[0].ToolCalls)
	}
}

func TestChatCompletionStreamMarksErrFinishReasonAsError(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{},"finish_reason":"error"}],"provider":"p1"}`,
	})
	defer srv.Close()

	c := New(srv.URL)
	deltas, err := c.ChatCompletionStream(context.Background(), "default", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := collectDeltas(t, deltas)

	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("expected exactly one delta carrying an Err, got %+v", got)
	}
}

func TestChatCompletionStreamSkipsMalformedChunks(t *testing.T) {
	srv := sseServer(t, []string{
		`{not valid json`,
		`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
	})
	defer srv.Close()

	c := New(srv.URL)
	deltas, err := c.ChatCompletionStream(context.Background(), "default", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := collectDeltas(t, deltas)

	if len(got) != 1 || got[0].Content != "ok" {
		t.Errorf("expected the malformed chunk to be skipped and only the real one delivered, got %+v", got)
	}
}

func TestChatCompletionStreamReturnsErrorOnNon2xxBeforeStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ChatCompletionStream(context.Background(), "default", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "bad model") {
		t.Errorf("err = %v, want it to mention the upstream error message", err)
	}
}

func TestChatCompletionNoChoicesIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ChatCompletion(context.Background(), "default", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Errorf("err = %v, want a \"no choices\" error", err)
	}
}

func TestChatCompletionDecodeErrorOnMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ChatCompletion(context.Background(), "default", nil, nil)
	if err == nil {
		t.Error("expected a decode error for malformed JSON")
	}
}

func TestChatCompletionPlainErrorWhenNon2xxHasNoJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ChatCompletion(context.Background(), "default", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want it to mention the raw status", err)
	}
}
