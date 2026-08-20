package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func recvMessage(t *testing.T, ch <-chan message) message {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("transport closed before message")
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
		return message{}
	}
}

func TestHTTPTransportJSONSessionHeadersAndAccepted(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") || r.Header.Get("X-Test") != "yes" {
			t.Errorf("headers=%v", r.Header)
		}
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Mcp-Session-Id", "session-1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "session-1" {
			t.Errorf("session not echoed: %v", r.Header)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	tr := newHTTPTransport(srv.URL, map[string]string{"X-Test": "yes"})
	if err := tr.Send(context.Background(), []byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	m := recvMessage(t, tr.Recv())
	if m.ID == nil || *m.ID != 1 || len(m.Result) == 0 {
		t.Fatalf("message=%#v", m)
	}
	if err := tr.Send(context.Background(), []byte(`{"method":"notice"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-tr.Recv(); ok {
		t.Fatal("channel should close")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPTransportSSEFiltersAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = w.Write([]byte("event: message\ndata: \n\ndata: nope\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notice\"}\n\ndata: [DONE]\n"))
		case "/badjson":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("not-json"))
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(" upstream exploded "))
		}
	}))
	defer srv.Close()
	tr := newHTTPTransport(srv.URL+"/sse", nil)
	if err := tr.Send(context.Background(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if m := recvMessage(t, tr.Recv()); m.Method != "notice" {
		t.Fatalf("message=%#v", m)
	}
	_ = tr.Close()
	bad := newHTTPTransport(srv.URL+"/error", nil)
	if err := bad.Send(context.Background(), []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("err=%v", err)
	}
	_ = bad.Close()
	malformed := newHTTPTransport(srv.URL+"/badjson", nil)
	if err := malformed.Send(context.Background(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	_ = malformed.Close()
	invalidURL := newHTTPTransport("://bad", nil)
	if err := invalidURL.Send(context.Background(), nil); err == nil {
		t.Fatal("invalid URL should fail")
	}
	_ = invalidURL.Close()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	network := newHTTPTransport(srv.URL, nil)
	if err := network.Send(canceled, nil); err == nil {
		t.Fatal("canceled request should fail")
	}
	_ = network.Close()
}

func TestStdioTransportRoundTripSkipsNoise(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The helper emits noise and blank lines, then echoes exactly one request.
	tr, err := newStdioTransport(ctx, "sh", []string{"-c", `printf 'noise\n\n'; IFS= read -r line; printf '%s\n' "$line"`}, map[string]string{"KRAM_MCP_TEST": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(request{JSONRPC: jsonRPCVersion, ID: 7, Method: "ping"})
	if err := tr.Send(ctx, payload); err != nil {
		t.Fatal(err)
	}
	m := recvMessage(t, tr.Recv())
	if m.ID == nil || *m.ID != 7 || m.Method != "ping" {
		t.Fatalf("message=%#v", m)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newStdioTransport(ctx, "/definitely/missing/kram-mcp", nil, nil); err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("err=%v", err)
	}
}
