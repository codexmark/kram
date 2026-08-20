package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnectHTTPHandshakeToolsAndIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var initialized bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		switch req.Method {
		case "initialize":
			if p, ok := req.Params.(map[string]any); ok && p["protocolVersion"] == "" {
				t.Error("missing protocol version")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"test-server","version":"1.2"}}}`))
		case "notifications/initialized":
			initialized = true
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo","description":"echoes","inputSchema":{"type":"object"}}]}}`))
		default:
			t.Errorf("unexpected method %q", req.Method)
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	c, err := Connect(context.Background(), "test", ServerConfig{Type: "http", URL: srv.URL, Headers: map[string]string{"X-Test": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !initialized || c.ServerInfo() != "test-server 1.2" || len(c.Tools()) != 1 || c.Tools()[0].Name != "echo" {
		t.Fatalf("initialized=%v info=%q tools=%#v", initialized, c.ServerInfo(), c.Tools())
	}
}

func TestConnectConfigurationAndHandshakeFailures(t *testing.T) {
	if _, err := Connect(context.Background(), "none", ServerConfig{}); err == nil || !strings.Contains(err.Error(), "needs either") {
		t.Fatalf("err=%v", err)
	}
	if _, err := Connect(context.Background(), "missing", ServerConfig{Command: "/definitely/missing"}); err == nil {
		t.Fatal("missing stdio command should fail")
	}
	for _, tc := range []struct {
		name     string
		handler  http.HandlerFunc
		contains string
	}{
		{"initialize RPC error", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"no init"}}`))
		}, "no init"},
		{"tools RPC error", func(w http.ResponseWriter, r *http.Request) {
			var req request
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "initialize" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"x"}}}`))
				return
			}
			if req.Method == "notifications/initialized" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-2,"message":"no tools"}}`))
		}, "no tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := Connect(context.Background(), "bad", ServerConfig{URL: srv.URL})
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
