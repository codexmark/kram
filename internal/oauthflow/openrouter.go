// Package oauthflow runs each provider's browser-login PKCE flow end to
// end: generate a PKCE pair, send the user's browser to the provider's
// own login/consent page, catch the redirect on a local callback server,
// and exchange the code for a credential.
//
// OpenRouter's flow (this file) is the simple case: the exchange returns
// a permanent API key, used exactly like a pasted one. Anthropic's
// (anthropic.go) and OpenAI's (openai.go) flows instead return a
// short-lived access token plus a refresh token — see Token in token.go
// — because they authenticate a subscription (Claude Pro/Max, ChatGPT
// Plus/Pro), not a developer API key. Gemini and OpenCode Zen have no
// OAuth flow at all; the accounts screen falls back to pasting a key for
// those.
package oauthflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	openRouterAuthURL     = "https://openrouter.ai/auth"
	openRouterExchangeURL = "https://openrouter.ai/api/v1/auth/keys"
	// callbackTimeout is how long the local server waits for the user to
	// finish the browser flow before giving up — generous, since it's a
	// human clicking through a login page, but bounded so a session
	// doesn't hang forever if they close the tab instead.
	callbackTimeout = 5 * time.Minute
)

// OpenRouterAuthorize starts the local callback listener and returns the
// authorization URL to open in a browser, plus a function that blocks
// until the flow completes (or times out/fails) and returns the real API
// key. Split into two steps so the caller (the CLI) can show the URL
// immediately and only block on wait() from a background command.
func OpenRouterAuthorize() (authURL string, wait func(ctx context.Context) (string, error), err error) {
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
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("callback had no code (error=%s)", r.URL.Query().Get("error"))
			http.Error(w, "authorization failed — no code returned, you can close this tab", http.StatusBadRequest)
			return
		}
		codeCh <- code
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body>Kram: OpenRouter connected. You can close this tab.</body></html>")
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)

	authURL = fmt.Sprintf("%s?callback_url=%s&code_challenge=%s&code_challenge_method=S256",
		openRouterAuthURL, callbackURL, challenge)

	wait = func(ctx context.Context) (string, error) {
		defer srv.Close()
		select {
		case code := <-codeCh:
			return exchangeCode(ctx, code, verifier)
		case err := <-errCh:
			return "", err
		case <-time.After(callbackTimeout):
			return "", fmt.Errorf("timed out waiting for OpenRouter authorization")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return authURL, wait, nil
}

func exchangeCode(ctx context.Context, code, verifier string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"code": code, "code_verifier": verifier, "code_challenge_method": "S256",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterExchangeURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchanging authorization code: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Key   string `json:"key"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.Key == "" {
		if out.Error != "" {
			return "", fmt.Errorf("OpenRouter rejected the code: %s", out.Error)
		}
		return "", fmt.Errorf("OpenRouter exchange failed (status %d)", resp.StatusCode)
	}
	return out.Key, nil
}
