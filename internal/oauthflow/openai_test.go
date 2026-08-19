package oauthflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func withOpenAIAuthBase(t *testing.T, base string) {
	t.Helper()
	orig := openAIAuthBase
	openAIAuthBase = base
	t.Cleanup(func() { openAIAuthBase = orig })
}

func TestExchangeOpenAICodeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "the-code" {
			t.Errorf("form = %v, want grant_type=authorization_code code=the-code", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"atok","refresh_token":"rtok","expires_in":7200}`))
	}))
	defer srv.Close()
	withOpenAIAuthBase(t, srv.URL)

	tok, err := exchangeOpenAICode(context.Background(), "the-code", "http://localhost:1455/auth/callback", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "atok" || tok.Refresh != "rtok" {
		t.Errorf("tok = %+v, want Access=atok Refresh=rtok", tok)
	}
	if time.Until(tok.ExpiresAt) < time.Hour {
		t.Errorf("ExpiresAt = %v, want roughly 2 hours from now", tok.ExpiresAt)
	}
}

func TestRefreshOpenAISuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
			t.Errorf("form = %v, want grant_type=refresh_token refresh_token=old-refresh", r.Form)
		}
		w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer srv.Close()
	withOpenAIAuthBase(t, srv.URL)

	tok, err := RefreshOpenAI(context.Background(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "new-access" {
		t.Errorf("Access = %q, want %q", tok.Access, "new-access")
	}
}

func TestRefreshOpenAIDefaultsExpiryWhenMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"atok"}`)) // no expires_in
	}))
	defer srv.Close()
	withOpenAIAuthBase(t, srv.URL)

	tok, err := RefreshOpenAI(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(tok.ExpiresAt) < 30*time.Minute {
		t.Errorf("ExpiresAt = %v, want the 3600s default to still apply", tok.ExpiresAt)
	}
}

func TestDoOpenAITokenRequestPropagatesProviderRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token expired"}`))
	}))
	defer srv.Close()
	withOpenAIAuthBase(t, srv.URL)

	_, err := RefreshOpenAI(context.Background(), "expired")
	if err == nil || !strings.Contains(err.Error(), "refresh token expired") {
		t.Errorf("err = %v, want it to mention the provider's rejection reason", err)
	}
}

// TestOpenAIAuthorizeFullRoundTrip drives the whole flow without a real
// browser, same technique as the Anthropic/OpenRouter round-trip tests —
// direct HTTP requests standing in for the browser's redirect.
func TestOpenAIAuthorizeFullRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"atok","refresh_token":"rtok","expires_in":3600}`))
	}))
	defer srv.Close()
	withOpenAIAuthBase(t, srv.URL)

	authURL, wait, err := OpenAIAuthorize()
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

	tok, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "atok" {
		t.Errorf("Access = %q, want %q", tok.Access, "atok")
	}
}

func TestOpenAIAuthorizeCallbackRejectsStateMismatch(t *testing.T) {
	authURL, wait, err := OpenAIAuthorize()
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	redirectURI := parsed.Query().Get("redirect_uri")

	go func() {
		time.Sleep(10 * time.Millisecond)
		resp, err := http.Get(redirectURI + "?code=c&state=wrong")
		if err == nil {
			resp.Body.Close()
		}
	}()

	_, err = wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("err = %v, want a state-mismatch error", err)
	}
}

func TestOpenAIAuthorizeCallbackPropagatesProviderError(t *testing.T) {
	authURL, wait, err := OpenAIAuthorize()
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
