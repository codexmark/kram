// Anthropic's browser-login flow, unlike OpenAI's (openai.go), is not
// something extracted from a real shipping client — no local reference
// implementation of it was found anywhere (see DECISIONS.md). The
// client_id below is publicly known only because it has been
// reverse-engineered from Anthropic's own official `claude` CLI by
// outside projects; Anthropic does not document a public OAuth client for
// third-party tools. It is real and has been used successfully elsewhere,
// but it is unofficial: it can break without warning if Anthropic ever
// rotates it, and using it is closer to "wearing the official CLI's
// clothes" than a sanctioned integration. Kram surfaces this honestly in
// the accounts screen rather than presenting it as first-class support —
// see accounts.go's "(beta, não oficial)" label on this option.
//
// Unlike OpenAI's ChatGPT login (openai.go), the OAuth access token here
// is not itself a usable inference credential — live-tested against a
// real Claude Pro/Max account: sending it directly as a Bearer token to
// /v1/messages or /v1/models is rejected with a scope permission_error
// (missing user:inference), even though it was requested. What the
// org:create_api_key scope is actually for, confirmed live against the
// same account, is a one-shot call to
// POST https://api.anthropic.com/api/oauth/claude_cli/create_api_key
// (Bearer: the OAuth access token), which mints a real, permanent,
// standard `sk-ant-...` API key — the same kind of credential the
// accounts screen already handles by pasting one in. So this flow
// exchanges the code once, immediately mints that key, and hands back a
// permanent string exactly like OpenRouterAuthorize does — there is no
// refresh path here, unlike OpenAI's, because nothing short-lived is
// ever stored.
package oauthflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	anthropicOAuthBase = "https://console.anthropic.com"
	anthropicAPIBase   = "https://api.anthropic.com"
	anthropicClientID  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	anthropicScope     = "org:create_api_key user:profile user:inference"
)

// AnthropicAuthorize starts the local callback listener and returns the
// authorization URL to open in a browser, plus a function that blocks
// until the flow completes and returns a real, permanent Anthropic API
// key. Same split-in-two shape as OpenRouterAuthorize.
func AnthropicAuthorize() (authURL string, wait func(ctx context.Context) (string, error), err error) {
	verifier, err := randomVerifier()
	if err != nil {
		return "", nil, fmt.Errorf("generating PKCE verifier: %w", err)
	}
	challenge := challengeFromVerifier(verifier)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("starting callback listener: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)

	state, err := randomVerifier()
	if err != nil {
		listener.Close()
		return "", nil, fmt.Errorf("generating state: %w", err)
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
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
		fmt.Fprint(w, "<html><body>Kram: Anthropic (Claude) connected. You can close this tab.</body></html>")
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)

	params := url.Values{
		"code":                  {"true"},
		"response_type":         {"code"},
		"client_id":             {anthropicClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {anthropicScope},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	authURL = anthropicOAuthBase + "/oauth/authorize?" + params.Encode()

	wait = func(ctx context.Context) (string, error) {
		defer srv.Close()
		select {
		case code := <-codeCh:
			tok, err := exchangeAnthropicCode(ctx, code, redirectURI, verifier, state)
			if err != nil {
				return "", err
			}
			return createAnthropicAPIKey(ctx, tok.Access)
		case err := <-errCh:
			return "", err
		case <-time.After(callbackTimeout):
			return "", fmt.Errorf("timed out waiting for Anthropic authorization")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return authURL, wait, nil
}

func exchangeAnthropicCode(ctx context.Context, code, redirectURI, verifier, state string) (Token, error) {
	body := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
		"client_id":     anthropicClientID,
		"code_verifier": verifier,
		"state":         state,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Token{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicOAuthBase+"/v1/oauth/token", bytes.NewReader(payload))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("Anthropic token request failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Token{}, fmt.Errorf("decoding Anthropic token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		if out.Error != "" {
			desc := out.ErrorDesc
			if desc == "" {
				desc = out.Error
			}
			return Token{}, fmt.Errorf("Anthropic rejected the request: %s", desc)
		}
		return Token{}, fmt.Errorf("Anthropic token exchange failed (status %d)", resp.StatusCode)
	}
	return Token{Access: out.AccessToken}, nil
}

// createAnthropicAPIKey spends the OAuth access token's org:create_api_key
// scope exactly once, minting a real API key on the account that just
// authorized Kram — live-verified against a real Claude Pro/Max account
// (see this file's package-level doc comment). The key comes back with
// expires_at: null (permanent), so nothing about this needs to be
// refreshed or re-derived later; it's stored and used exactly like a
// pasted key from here on.
func createAnthropicAPIKey(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPIBase+"/api/oauth/claude_cli/create_api_key", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating Anthropic API key: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		RawKey string `json:"raw_key"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding Anthropic create-key response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.RawKey == "" {
		if out.Error != "" {
			return "", fmt.Errorf("Anthropic rejected the create-key request: %s", out.Error)
		}
		return "", fmt.Errorf("Anthropic create-key request failed (status %d)", resp.StatusCode)
	}
	return out.RawKey, nil
}
