package oauthflow

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestRandomVerifierIsUniqueAndURLSafe(t *testing.T) {
	a, err := randomVerifier()
	if err != nil {
		t.Fatal(err)
	}
	b, err := randomVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two calls to randomVerifier produced the same value")
	}
	if _, err := base64.RawURLEncoding.DecodeString(a); err != nil {
		t.Errorf("verifier is not valid base64url: %v", err)
	}
}

func TestChallengeFromVerifierIsDeterministicSHA256(t *testing.T) {
	verifier := "fixed-test-verifier"
	got := challengeFromVerifier(verifier)

	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got != want {
		t.Errorf("challengeFromVerifier(%q) = %q, want %q", verifier, got, want)
	}

	// Same verifier must always produce the same challenge — PKCE
	// depends on this being deterministic, not random per call.
	if again := challengeFromVerifier(verifier); again != got {
		t.Error("challengeFromVerifier is not deterministic for the same input")
	}
}

func TestRefreshFuncKnowsRefreshableAccounts(t *testing.T) {
	if RefreshFunc("openai-chatgpt") == nil {
		t.Error("expected a refresh function for \"openai-chatgpt\"")
	}
	// OpenRouter's flow exchanges for a permanent key, and Anthropic's
	// exchanges its OAuth token for a permanent one via one extra
	// create-key call (see anthropic.go's doc comment) — neither has
	// anything left to refresh afterward.
	if RefreshFunc("openrouter") != nil {
		t.Error("expected no refresh function for \"openrouter\" (permanent key, nothing to refresh)")
	}
	if RefreshFunc("anthropic") != nil {
		t.Error("expected no refresh function for \"anthropic\" (exchanges for a permanent key, nothing to refresh)")
	}
	if RefreshFunc("unknown-account") != nil {
		t.Error("expected no refresh function for an unknown account ID")
	}
}
