package oauthflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func withAnthropicBases(t *testing.T, oauthBase, apiBase string) {
	t.Helper()
	origOAuth, origAPI := anthropicOAuthBase, anthropicAPIBase
	if oauthBase != "" {
		anthropicOAuthBase = oauthBase
	}
	if apiBase != "" {
		anthropicAPIBase = apiBase
	}
	t.Cleanup(func() { anthropicOAuthBase, anthropicAPIBase = origOAuth, origAPI })
}

func TestExchangeAnthropicCodeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "the-code" || body["code_verifier"] != "the-verifier" {
			t.Errorf("request body = %+v, want code=the-code code_verifier=the-verifier", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "atok"})
	}))
	defer srv.Close()
	withAnthropicBases(t, srv.URL, "")

	tok, err := exchangeAnthropicCode(context.Background(), "the-code", "http://localhost:1/callback", "the-verifier", "state1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "atok" {
		t.Errorf("Access = %q, want %q", tok.Access, "atok")
	}
}

func TestExchangeAnthropicCodeProviderRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "code expired"})
	}))
	defer srv.Close()
	withAnthropicBases(t, srv.URL, "")

	_, err := exchangeAnthropicCode(context.Background(), "stale-code", "http://localhost:1/callback", "v", "s")
	if err == nil || !strings.Contains(err.Error(), "code expired") {
		t.Errorf("err = %v, want it to mention the provider's error description", err)
	}
}

func TestExchangeAnthropicCodeFailsWithoutErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()
	withAnthropicBases(t, srv.URL, "")

	_, err := exchangeAnthropicCode(context.Background(), "c", "http://localhost:1/callback", "v", "s")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want it to mention the raw status when no error field is present", err)
	}
}

func TestCreateAnthropicAPIKeySuccess(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{"raw_key": "sk-ant-real"})
	}))
	defer srv.Close()
	withAnthropicBases(t, "", srv.URL)

	key, err := createAnthropicAPIKey(context.Background(), "my-access-token")
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-ant-real" {
		t.Errorf("key = %q, want %q", key, "sk-ant-real")
	}
	if gotAuth != "Bearer my-access-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer my-access-token")
	}
}

func TestCreateAnthropicAPIKeyRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing scope org:create_api_key"})
	}))
	defer srv.Close()
	withAnthropicBases(t, "", srv.URL)

	_, err := createAnthropicAPIKey(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "missing scope") {
		t.Errorf("err = %v, want it to mention the rejection reason", err)
	}
}

// TestAnthropicAuthorizeFullRoundTrip drives the entire flow without a
// real browser: OAuth callback is simulated with a direct HTTP GET to
// the local redirect URI (exactly what a browser redirect would do),
// and both the token-exchange and create-key calls are pointed at one
// combined httptest.Server via the overridable base-URL vars.
func TestAnthropicAuthorizeFullRoundTrip(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "atok"})
	})
	mux.HandleFunc("/api/oauth/claude_cli/create_api_key", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer atok" {
			t.Errorf("create-key Authorization = %q, want Bearer atok", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"raw_key": "sk-ant-final"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withAnthropicBases(t, srv.URL, srv.URL)

	authURL, wait, err := AnthropicAuthorize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	redirectURI, state := q.Get("redirect_uri"), q.Get("state")

	go func() {
		time.Sleep(10 * time.Millisecond)
		resp, err := http.Get(redirectURI + "?code=real-code&state=" + state)
		if err == nil {
			resp.Body.Close()
		}
	}()

	key, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-ant-final" {
		t.Errorf("key = %q, want %q", key, "sk-ant-final")
	}
}

func TestAnthropicAuthorizeCallbackRejectsStateMismatch(t *testing.T) {
	authURL, wait, err := AnthropicAuthorize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	redirectURI := parsed.Query().Get("redirect_uri")

	go func() {
		time.Sleep(10 * time.Millisecond)
		resp, err := http.Get(redirectURI + "?code=c&state=wrong-state")
		if err == nil {
			resp.Body.Close()
		}
	}()

	_, err = wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("err = %v, want a state-mismatch error", err)
	}
}

func TestAnthropicAuthorizeCallbackPropagatesProviderError(t *testing.T) {
	authURL, wait, err := AnthropicAuthorize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	redirectURI := parsed.Query().Get("redirect_uri")

	go func() {
		time.Sleep(10 * time.Millisecond)
		resp, err := http.Get(redirectURI + "?error=access_denied&error_description=user+declined")
		if err == nil {
			resp.Body.Close()
		}
	}()

	_, err = wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "user declined") {
		t.Errorf("err = %v, want it to mention the provider's denial reason", err)
	}
}
