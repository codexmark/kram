package oauthflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// openAIAuthBase is a var, not a const, so tests can point this flow's
// HTTP calls at an httptest.Server instead of the real OpenAI endpoint —
// nothing in production ever reassigns it.
var openAIAuthBase = "https://auth.openai.com"

const (
	// openAIClientID is OpenAI's own public OAuth client for "log in with
	// ChatGPT" — confirmed by extracting it directly from a real shipping
	// opencode build (same client_id, same endpoints, same flow shape
	// found there), not guessed or reverse-engineered from documentation
	// that doesn't exist. It authenticates a ChatGPT Plus/Pro/Team
	// subscription, not the standard OpenAI developer API — the resulting
	// token only works against the Codex Responses backend (see
	// internal/provider/openai_responses.go), never against
	// api.openai.com.
	openAIClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// openAICallbackPort is fixed, not ephemeral — unlike OpenRouter's and
	// Anthropic's flows, which accept any localhost port. OpenAI's
	// authorization server validates redirect_uri against an allowlist
	// registered for this client_id, and the opencode client this was
	// extracted from hardcodes this exact port (not a dynamically chosen
	// one) — confirmed live: an authorize request with a random port was
	// rejected outright as "invalid_authorize_request" before this fix.
	// If something else already owns this port locally, the flow fails
	// to start with a clear error rather than picking a different,
	// silently-rejected one.
	openAICallbackPort = 1455
)

// OpenAIAuthorize starts the local callback listener and returns the
// authorization URL to open in a browser, plus a function that blocks
// until the flow completes and returns the ChatGPT-subscription token
// pair. Mirrors OpenRouterAuthorize's split-in-two shape (see
// openrouter.go) for the same reason: the caller shows the URL
// immediately and only blocks on wait() from a background command.
func OpenAIAuthorize() (authURL string, wait func(ctx context.Context) (Token, error), err error) {
	verifier, err := randomVerifier()
	if err != nil {
		return "", nil, fmt.Errorf("generating PKCE verifier: %w", err)
	}
	challenge := challengeFromVerifier(verifier)

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", openAICallbackPort))
	if err != nil {
		return "", nil, fmt.Errorf("starting callback listener on port %d (required — OpenAI only allows this exact redirect URI for this client): %w", openAICallbackPort, err)
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", openAICallbackPort)

	state, err := randomVerifier()
	if err != nil {
		listener.Close()
		return "", nil, fmt.Errorf("generating state: %w", err)
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			desc := q.Get("error_description")
			if desc == "" {
				desc = e
			}
			errCh <- fmt.Errorf("authorization denied: %s", desc)
			http.Error(w, "authorization failed — you can close this tab", http.StatusBadRequest)
			return
		}
		if q.Get("state") != state {
			errCh <- fmt.Errorf("callback state mismatch (possible CSRF)")
			http.Error(w, "invalid state — you can close this tab", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" {
			errCh <- fmt.Errorf("callback had no authorization code")
			http.Error(w, "no code returned — you can close this tab", http.StatusBadRequest)
			return
		}
		codeCh <- code
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body>Kram: OpenAI (ChatGPT) connected. You can close this tab.</body></html>")
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)

	params := url.Values{
		"response_type":              {"code"},
		"client_id":                  {openAIClientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {"openid profile email offline_access"},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {state},
		"originator":                 {"kram"},
	}
	authURL = openAIAuthBase + "/oauth/authorize?" + params.Encode()

	wait = func(ctx context.Context) (Token, error) {
		defer srv.Close()
		select {
		case code := <-codeCh:
			return exchangeOpenAICode(ctx, code, redirectURI, verifier)
		case err := <-errCh:
			return Token{}, err
		case <-time.After(callbackTimeout):
			return Token{}, fmt.Errorf("timed out waiting for OpenAI authorization")
		case <-ctx.Done():
			return Token{}, ctx.Err()
		}
	}
	return authURL, wait, nil
}

// RefreshOpenAI exchanges a still-valid refresh token for a new access
// token, without involving the browser again.
func RefreshOpenAI(ctx context.Context, refreshToken string) (Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {openAIClientID},
	}
	return doOpenAITokenRequest(ctx, form)
}

func exchangeOpenAICode(ctx context.Context, code, redirectURI, verifier string) (Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {openAIClientID},
		"code_verifier": {verifier},
	}
	return doOpenAITokenRequest(ctx, form)
}

func doOpenAITokenRequest(ctx context.Context, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIAuthBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("OpenAI token request failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Token{}, fmt.Errorf("decoding OpenAI token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		if out.Error != "" {
			desc := out.ErrorDesc
			if desc == "" {
				desc = out.Error
			}
			return Token{}, fmt.Errorf("OpenAI rejected the request: %s", desc)
		}
		return Token{}, fmt.Errorf("OpenAI token exchange failed (status %d)", resp.StatusCode)
	}

	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return Token{
		Access:    out.AccessToken,
		Refresh:   out.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second),
	}, nil
}
