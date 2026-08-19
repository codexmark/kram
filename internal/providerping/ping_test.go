package providerping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPingOKForOpenAICompat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("expected Authorization: Bearer sk-test, got %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/models" {
			t.Errorf("expected GET /models, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "openai-compat", srv.URL, "sk-test")
	if res.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK; detail=%q", res.Status, res.Detail)
	}
}

func TestPingAnthropicHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant" {
			t.Errorf("expected x-api-key: sk-ant, got %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("expected an anthropic-version header")
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("expected GET /v1/models, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "anthropic", srv.URL, "sk-ant")
	if res.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK", res.Status)
	}
}

func TestPingOpenAIResponsesUsesBearer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer sk-chatgpt" {
			t.Errorf("expected Authorization: Bearer sk-chatgpt, got %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "openai-responses", srv.URL, "sk-chatgpt")
	if res.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK", res.Status)
	}
}

func TestPingGeminiKeyAsQueryParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "gk-test" {
			t.Errorf("expected ?key=gk-test, got %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "gemini", srv.URL, "gk-test")
	if res.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK", res.Status)
	}
}

func TestPingRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "openai-compat", srv.URL, "x")
	if res.Status != StatusDown {
		t.Errorf("Status = %v, want StatusDown for a 429", res.Status)
	}
	if res.Detail == "" {
		t.Error("expected a non-empty detail explaining the 429")
	}
}

func TestPingUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "openai-compat", srv.URL, "bad-key")
	if res.Status != StatusDown {
		t.Errorf("Status = %v, want StatusDown for a 401", res.Status)
	}
}

func TestPingServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "openai-compat", srv.URL, "x")
	if res.Status != StatusDown {
		t.Errorf("Status = %v, want StatusDown for a 500", res.Status)
	}
}

func TestPingUnreachable(t *testing.T) {
	res := Ping(context.Background(), "openai-compat", "http://127.0.0.1:1", "x")
	if res.Status != StatusDown {
		t.Errorf("Status = %v, want StatusDown for an unreachable host", res.Status)
	}
}

func TestPingDegradedOnHighLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(degradedLatency + 100*time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "openai-compat", srv.URL, "x")
	if res.Status != StatusDegraded {
		t.Errorf("Status = %v, want StatusDegraded for a slow-but-successful response", res.Status)
	}
}

func TestPingRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	res := Ping(ctx, "openai-compat", srv.URL, "x")
	if res.Status != StatusDown {
		t.Errorf("Status = %v, want StatusDown when the context is canceled before a response arrives", res.Status)
	}
}
