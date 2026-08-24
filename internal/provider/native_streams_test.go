package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codexmark/kram/internal/openai"
)

func assertHTTPError(t *testing.T, err error, id string) {
	t.Helper()
	var he *HTTPError
	if !errors.As(err, &he) || he.Provider != id || he.StatusCode != 429 {
		t.Fatalf("error=%T %v", err, err)
	}
}

func TestAnthropicFullTextUsageAndHTTPFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") != "" {
			w.WriteHeader(429)
			return
		}
		if r.Header.Get("x-api-key") != "key" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{"not-json", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`, `{"type":"content_block_delta","index":5,"delta":{"text":"hello"}}`, `{"type":"message_delta","usage":{"output_tokens":4}}`, `{"type":"message_stop"}`}
		for _, line := range lines {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
	}))
	defer srv.Close()
	p := NewAnthropic("a", srv.URL, "key", "pinned", capabilities{})
	max := 12
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{Model: "ignored", MaxTokens: &max})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if len(got) != 2 || got[0].Delta != "hello" || !got[1].Done || got[1].Usage.TotalTokens != 7 {
		t.Fatalf("events=%#v", got)
	}
	fail := NewAnthropic("a", srv.URL+"?fail=1", "key", "", capabilities{})
	_, err = fail.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	assertHTTPError(t, err, "a")
	invalid := NewAnthropic("a", "://bad", "key", "", capabilities{})
	if _, err := invalid.ChatCompletion(context.Background(), openai.ChatCompletionRequest{}); err == nil || !strings.Contains(err.Error(), "building request") {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiFullTextToolUsageAndFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fail") {
			w.WriteHeader(429)
			return
		}
		if r.URL.Query().Get("key") != "key" || r.URL.Query().Get("alt") != "sse" {
			t.Errorf("query=%v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: nope\n\n")
		fmt.Fprint(w, `data: {"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5},"candidates":[{"content":{"parts":[{"text":"hi"},{"functionCall":{"name":"tool"},"thoughtSignature":"sig"}]}}]}`+"\n\n")
	}))
	defer srv.Close()
	p := NewGemini("g", srv.URL, "key", "model", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	if len(got) != 3 || got[0].Delta != "hi" || !got[1].ToolCallProgress || !got[2].Done || got[2].Usage.TotalTokens != 5 || got[2].ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("events=%#v", got)
	}
	fail := NewGemini("g", srv.URL+"/fail", "key", "model", capabilities{})
	_, err = fail.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	assertHTTPError(t, err, "g")
	invalid := NewGemini("g", "://bad", "key", "model", capabilities{})
	if _, err := invalid.ChatCompletion(context.Background(), openai.ChatCompletionRequest{}); err == nil || !strings.Contains(err.Error(), "building request") {
		t.Fatalf("err=%v", err)
	}
}

func TestResponsesFullStreamResolverAndFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") != "" {
			w.WriteHeader(429)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		lines := []string{"bad", `{"type":"response.output_text.delta","delta":"ok"}`, `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc1","type":"function_call","call_id":"c1","name":"tool","arguments":"{"}}`, `{"type":"response.function_call_arguments.delta","item_id":"fc1","output_index":0,"delta":"}"}`, `{"type":"response.completed","response":{"usage":{"input_tokens":6,"output_tokens":7}}}`}
		for _, line := range lines {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
	}))
	defer srv.Close()
	resolve := func(context.Context) (string, error) { return "token", nil }
	p := NewOpenAIResponses("r", srv.URL, resolve, "model", capabilities{})
	events, err := p.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := collectEvents(t, events)
	last := got[len(got)-1]
	if got[0].Delta != "ok" || !last.Done || last.Usage.TotalTokens != 13 || len(last.ToolCalls) != 1 || last.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("events=%#v", got)
	}
	badResolve := NewOpenAIResponses("r", srv.URL, func(context.Context) (string, error) { return "", errors.New("expired") }, "", capabilities{})
	if _, err := badResolve.ChatCompletion(context.Background(), openai.ChatCompletionRequest{}); err == nil || !strings.Contains(err.Error(), "resolving credential") {
		t.Fatalf("err=%v", err)
	}
	fail := NewOpenAIResponses("r", srv.URL+"?fail=1", resolve, "", capabilities{})
	_, err = fail.ChatCompletion(context.Background(), openai.ChatCompletionRequest{})
	assertHTTPError(t, err, "r")
	invalid := NewOpenAIResponses("r", "://bad", resolve, "", capabilities{})
	if _, err := invalid.ChatCompletion(context.Background(), openai.ChatCompletionRequest{}); err == nil || !strings.Contains(err.Error(), "building request") {
		t.Fatalf("err=%v", err)
	}
}
