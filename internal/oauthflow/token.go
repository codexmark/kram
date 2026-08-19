package oauthflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"time"
)

// Token is a refreshable OAuth credential: a short-lived access token plus
// a refresh token that can mint a new one once Access expires. This is
// the shape OpenAI's ChatGPT browser login returns (see openai.go) —
// unlike OpenRouter's and Anthropic's flows, both of which exchange once
// for a permanent credential (a developer API key either directly, or
// via one extra create-key call for Anthropic — see anthropic.go's doc
// comment) and have no ongoing use for this type.
type Token struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time
}

// randomVerifier and challengeFromVerifier implement PKCE (RFC 7636) —
// shared by every flow in this package (OpenRouter, Anthropic, OpenAI),
// each of which sends the browser to a provider's own authorize page and
// proves possession of the verifier when exchanging the returned code.
func randomVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func challengeFromVerifier(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// RefreshFunc returns the refresh function for a providercatalog account
// ID, or nil if that account has no refreshable OAuth flow — true for
// OpenRouter and Anthropic both, which exchange for a permanent
// credential with nothing to refresh (see their own files' doc
// comments), so only OpenAI's ChatGPT login is present here. Centralizes
// the account-ID → refresh-function mapping so callers (the CLI's ping
// command, the gateway's provider wiring) don't each hardcode their own
// copy of it.
func RefreshFunc(acctID string) func(ctx context.Context, refreshToken string) (Token, error) {
	switch acctID {
	case "openai-chatgpt":
		return RefreshOpenAI
	default:
		return nil
	}
}
