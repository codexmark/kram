package oauthflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// extractQueryParamRaw pulls a query parameter's raw (possibly
// unescaped) value out of a URL string — OpenRouterAuthorize builds its
// authURL by hand with fmt.Sprintf rather than url.Values.Encode(), so
// its callback_url value isn't percent-encoded and a strict url.Parse
// round-trip isn't guaranteed; this matches what the real query string
// actually contains instead.
func extractQueryParamRaw(t *testing.T, rawURL, key string) string {
	t.Helper()
	marker := key + "="
	i := strings.Index(rawURL, marker)
	if i < 0 {
		t.Fatalf("query param %q not found in %q", key, rawURL)
	}
	rest := rawURL[i+len(marker):]
	if j := strings.IndexByte(rest, '&'); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestOpenRouterAuthorizeFullRoundTrip(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "auth-code-123" {
			t.Errorf("exchange body code = %q, want %q", body["code"], "auth-code-123")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "sk-or-real-key"})
	}))
	defer exchangeSrv.Close()

	origExchange := openRouterExchangeURL
	openRouterExchangeURL = exchangeSrv.URL
	t.Cleanup(func() { openRouterExchangeURL = origExchange })

	authURL, wait, err := OpenRouterAuthorize()
	if err != nil {
		t.Fatal(err)
	}
	callbackURL := extractQueryParamRaw(t, authURL, "callback_url")

	go func() {
		time.Sleep(10 * time.Millisecond)
		resp, err := http.Get(callbackURL + "?code=auth-code-123")
		if err == nil {
			resp.Body.Close()
		}
	}()

	key, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-or-real-key" {
		t.Errorf("key = %q, want %q", key, "sk-or-real-key")
	}
}

func TestOpenRouterAuthorizeCallbackWithNoCodeFails(t *testing.T) {
	authURL, wait, err := OpenRouterAuthorize()
	if err != nil {
		t.Fatal(err)
	}
	callbackURL := extractQueryParamRaw(t, authURL, "callback_url")

	go func() {
		time.Sleep(10 * time.Millisecond)
		resp, err := http.Get(callbackURL + "?error=access_denied")
		if err == nil {
			resp.Body.Close()
		}
	}()

	_, err = wait(context.Background())
	if err == nil {
		t.Error("expected an error when the callback carries no code")
	}
}

func TestOpenRouterAuthorizeExchangeRejection(t *testing.T) {
	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid code_verifier"})
	}))
	defer exchangeSrv.Close()

	origExchange := openRouterExchangeURL
	openRouterExchangeURL = exchangeSrv.URL
	t.Cleanup(func() { openRouterExchangeURL = origExchange })

	authURL, wait, err := OpenRouterAuthorize()
	if err != nil {
		t.Fatal(err)
	}
	callbackURL := extractQueryParamRaw(t, authURL, "callback_url")

	go func() {
		time.Sleep(10 * time.Millisecond)
		resp, err := http.Get(callbackURL + "?code=some-code")
		if err == nil {
			resp.Body.Close()
		}
	}()

	_, err = wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid code_verifier") {
		t.Errorf("err = %v, want it to mention the exchange server's rejection reason", err)
	}
}

func TestOpenRouterAuthorizeWaitRespectsContextCancellation(t *testing.T) {
	_, wait, err := OpenRouterAuthorize()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = wait(ctx)
	if err == nil {
		t.Error("expected wait() to return promptly with an error when its context is already cancelled")
	}
}
