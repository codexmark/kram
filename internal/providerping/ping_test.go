package providerping

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// TestPingOpenAIResponsesBodyErrorIsReachable is the regression test for the
// false-negative red dot on a working ChatGPT login: the Codex backend
// rejects this probe's deliberately-minimal body with a 400 ("Store must be
// set to false"), which means auth SUCCEEDED — so it must read as reachable
// (StatusOK), not down.
func TestPingOpenAIResponsesBodyErrorIsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Store must be set to false"}`))
	}))
	defer srv.Close()

	res := Ping(context.Background(), "openai-responses", srv.URL, "valid-token")
	if res.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK for a Codex 400 body-validation error (auth passed)", res.Status)
	}
}

// TestPingOpenAIResponsesAuthFailureIsDown confirms a genuine auth failure
// (401) still shows down — the body-error leniency above must not swallow it.
func TestPingOpenAIResponsesAuthFailureIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := Ping(context.Background(), "openai-responses", srv.URL, "bad-token")
	if res.Status != StatusDown || res.Detail != "invalid key" {
		t.Errorf("Status/Detail = %v/%q, want StatusDown/\"invalid key\" for a 401", res.Status, res.Detail)
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

func TestListModelsReturnsSortedIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("expected GET /models, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"qwen3.5-9b"},{"id":"prism-ml/bonsai-27b"},{"id":""}]}`)
	}))
	defer srv.Close()

	got, err := ListModels(context.Background(), srv.URL, "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"prism-ml/bonsai-27b", "qwen3.5-9b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListModels = %v, want %v (sorted, empty ID dropped)", got, want)
	}
}

func TestListModelsNoAuthHeaderWhenKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want no header for a no-auth local server", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"llama-3"}]}`)
	}))
	defer srv.Close()

	got, err := ListModels(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "llama-3" {
		t.Errorf("ListModels = %v, want [llama-3]", got)
	}
}

func TestListModelsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := ListModels(context.Background(), srv.URL, "bad-key")
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
}

func TestListModelsMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()

	_, err := ListModels(context.Background(), srv.URL, "")
	if err == nil {
		t.Fatal("expected an error for a malformed response body")
	}
}

func TestListModelsUnreachable(t *testing.T) {
	_, err := ListModels(context.Background(), "http://127.0.0.1:1", "")
	if err == nil {
		t.Fatal("expected an error for an unreachable server")
	}
}
