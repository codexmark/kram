package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isolate points XDG_CONFIG_HOME at a fresh temp dir for the duration of
// one test — the same isolation this package needed by hand (via env -u)
// during manual testing, now automatic. Without it, a test touching this
// package risks reading or writing the real user's config.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load on a fresh dir should not error, got: %v", err)
	}
	if got := s.Get("ANTHROPIC_API_KEY"); got != "" {
		t.Errorf("expected empty string for an unset key, got %q", got)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("ANTHROPIC_API_KEY", "sk-test-123"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("ANTHROPIC_API_KEY"); got != "sk-test-123" {
		t.Errorf("got %q, want sk-test-123", got)
	}

	// A fresh Load must see what a previous Store instance persisted.
	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get("ANTHROPIC_API_KEY"); got != "sk-test-123" {
		t.Errorf("after reload, got %q, want sk-test-123", got)
	}
}

func TestDelete(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.Set("OPENAI_API_KEY", "sk-abc")
	if err := s.Delete("OPENAI_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("OPENAI_API_KEY"); got != "" {
		t.Errorf("expected empty after delete, got %q", got)
	}

	reloaded, _ := Load()
	if got := reloaded.Get("OPENAI_API_KEY"); got != "" {
		t.Errorf("delete should persist — after reload got %q", got)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := isolate(t)
	s, _ := Load()
	if err := s.Set("GEMINI_API_KEY", "sk-xyz"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "kram-gateway", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("credentials file should exist at %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials file permissions = %o, want 600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("credentials dir permissions = %o, want 700", perm)
	}
}

func TestAllReturnsACopy(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.Set("ANTHROPIC_API_KEY", "sk-1")

	all := s.All()
	all["ANTHROPIC_API_KEY"] = "tampered"

	if got := s.Get("ANTHROPIC_API_KEY"); got != "sk-1" {
		t.Errorf("mutating the map from All() should not affect the store, got %q", got)
	}
}

func TestOAuthSetGetDeleteRoundTrip(t *testing.T) {
	isolate(t)
	s, _ := Load()
	tok := OAuthToken{Access: "access-1", Refresh: "refresh-1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.SetOAuth("ANTHROPIC_API_KEY", tok); err != nil {
		t.Fatal(err)
	}

	got, ok := s.GetOAuth("ANTHROPIC_API_KEY")
	if !ok {
		t.Fatal("expected an OAuth token to be present")
	}
	if got.Access != tok.Access || got.Refresh != tok.Refresh {
		t.Errorf("got %+v, want %+v", got, tok)
	}

	// A fresh Load must see what a previous Store instance persisted, in
	// its own file separate from the plain-key credentials.json.
	reloaded, _ := Load()
	if got, ok := reloaded.GetOAuth("ANTHROPIC_API_KEY"); !ok || got.Access != tok.Access {
		t.Errorf("after reload, GetOAuth = %+v, %v; want %+v, true", got, ok, tok)
	}

	if err := s.DeleteOAuth("ANTHROPIC_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetOAuth("ANTHROPIC_API_KEY"); ok {
		t.Error("expected no OAuth token after delete")
	}
}

func TestOAuthTokenFilePermissions(t *testing.T) {
	dir := isolate(t)
	s, _ := Load()
	if err := s.SetOAuth("ANTHROPIC_API_KEY", OAuthToken{Access: "a", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "kram-gateway", "oauth_tokens.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("oauth token file should exist at %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("oauth token file permissions = %o, want 600", perm)
	}
}

func TestResolveFallsBackToPlainKeyWhenNoOAuthToken(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.Set("OPENROUTER_API_KEY", "sk-plain")

	got, err := s.Resolve(context.Background(), "OPENROUTER_API_KEY", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-plain" {
		t.Errorf("got %q, want sk-plain", got)
	}
}

func TestResolveErrorsWhenNothingStored(t *testing.T) {
	isolate(t)
	s, _ := Load()
	if _, err := s.Resolve(context.Background(), "ANTHROPIC_API_KEY", nil); err == nil {
		t.Error("expected an error resolving a credential that was never stored")
	}
}

func TestResolveReturnsAccessWithoutRefreshingWhenStillValid(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.SetOAuth("ANTHROPIC_API_KEY", OAuthToken{
		Access: "still-good", Refresh: "r1", ExpiresAt: time.Now().Add(time.Hour),
	})

	refreshCalled := false
	refresh := func(ctx context.Context, refreshToken string) (OAuthToken, error) {
		refreshCalled = true
		return OAuthToken{}, errors.New("should not be called")
	}

	got, err := s.Resolve(context.Background(), "ANTHROPIC_API_KEY", refresh)
	if err != nil {
		t.Fatal(err)
	}
	if got != "still-good" {
		t.Errorf("got %q, want still-good", got)
	}
	if refreshCalled {
		t.Error("refresh should not be called for a token well within its expiry")
	}
}

func TestResolveRefreshesAnExpiredToken(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.SetOAuth("ANTHROPIC_API_KEY", OAuthToken{
		Access: "stale", Refresh: "r1", ExpiresAt: time.Now().Add(-time.Minute),
	})

	refresh := func(ctx context.Context, refreshToken string) (OAuthToken, error) {
		if refreshToken != "r1" {
			t.Errorf("expected refresh token r1, got %q", refreshToken)
		}
		return OAuthToken{Access: "fresh", Refresh: "r2", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	got, err := s.Resolve(context.Background(), "ANTHROPIC_API_KEY", refresh)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fresh" {
		t.Errorf("got %q, want fresh", got)
	}

	// The refreshed token must be persisted, not just returned once.
	stored, ok := s.GetOAuth("ANTHROPIC_API_KEY")
	if !ok || stored.Access != "fresh" || stored.Refresh != "r2" {
		t.Errorf("stored token after refresh = %+v", stored)
	}
}

func TestResolveRefreshesWithinSkewWindow(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.SetOAuth("ANTHROPIC_API_KEY", OAuthToken{
		Access: "about-to-expire", Refresh: "r1", ExpiresAt: time.Now().Add(30 * time.Second),
	})

	refreshCalled := false
	refresh := func(ctx context.Context, refreshToken string) (OAuthToken, error) {
		refreshCalled = true
		return OAuthToken{Access: "fresh", Refresh: "r2", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	if _, err := s.Resolve(context.Background(), "ANTHROPIC_API_KEY", refresh); err != nil {
		t.Fatal(err)
	}
	if !refreshCalled {
		t.Error("expected refresh to be called for a token expiring within the skew window")
	}
}

func TestResolveKeepsOldRefreshTokenWhenNotRotated(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.SetOAuth("ANTHROPIC_API_KEY", OAuthToken{
		Access: "stale", Refresh: "original-refresh", ExpiresAt: time.Now().Add(-time.Minute),
	})

	// Some providers don't rotate the refresh token on every refresh —
	// the result carries an empty Refresh, which Resolve must treat as
	// "unchanged" rather than discarding the still-valid one.
	refresh := func(ctx context.Context, refreshToken string) (OAuthToken, error) {
		return OAuthToken{Access: "fresh", Refresh: "", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	if _, err := s.Resolve(context.Background(), "ANTHROPIC_API_KEY", refresh); err != nil {
		t.Fatal(err)
	}

	stored, _ := s.GetOAuth("ANTHROPIC_API_KEY")
	if stored.Refresh != "original-refresh" {
		t.Errorf("Refresh = %q, want original-refresh to be preserved", stored.Refresh)
	}
}

func TestResolveErrorsWhenRefreshFails(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.SetOAuth("ANTHROPIC_API_KEY", OAuthToken{
		Access: "stale", Refresh: "r1", ExpiresAt: time.Now().Add(-time.Minute),
	})

	refresh := func(ctx context.Context, refreshToken string) (OAuthToken, error) {
		return OAuthToken{}, errors.New("provider rejected the refresh token")
	}

	if _, err := s.Resolve(context.Background(), "ANTHROPIC_API_KEY", refresh); err == nil {
		t.Error("expected an error when the refresh call fails")
	}

	// The stale token must still be there — a failed refresh should not
	// have wiped it out.
	if _, ok := s.GetOAuth("ANTHROPIC_API_KEY"); !ok {
		t.Error("expected the stale token to remain stored after a failed refresh")
	}
}

func TestResolveErrorsWhenExpiredAndNoRefreshFunc(t *testing.T) {
	isolate(t)
	s, _ := Load()
	_ = s.SetOAuth("ANTHROPIC_API_KEY", OAuthToken{
		Access: "stale", Refresh: "r1", ExpiresAt: time.Now().Add(-time.Minute),
	})

	if _, err := s.Resolve(context.Background(), "ANTHROPIC_API_KEY", nil); err == nil {
		t.Error("expected an error resolving an expired token with no refresh function")
	}
}
