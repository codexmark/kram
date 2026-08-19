package provider

import (
	"context"
	"testing"

	"github.com/codexmark/kram/internal/config"
)

func TestBuildStaticAuthByKind(t *testing.T) {
	t.Setenv("FACTORY_TEST_KEY", "sk-test")

	cases := []struct {
		kind    string
		baseURL string
		wantErr bool
	}{
		{kind: "anthropic", wantErr: false},
		{kind: "gemini", wantErr: false},
		{kind: "openai-compat", baseURL: "http://127.0.0.1:9099", wantErr: false},
		{kind: "openai-compat", baseURL: "", wantErr: true}, // base_url required
		{kind: "unknown-kind", wantErr: true},
	}

	for _, tc := range cases {
		cfg := config.ProviderConfig{ID: "p", Kind: tc.kind, BaseURL: tc.baseURL, APIKeyEnv: "FACTORY_TEST_KEY"}
		p, err := Build(cfg, nil)
		if tc.wantErr {
			if err == nil {
				t.Errorf("kind=%q base_url=%q: expected an error, got provider %+v", tc.kind, tc.baseURL, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("kind=%q: unexpected error: %v", tc.kind, err)
			continue
		}
		if p.Kind() != tc.kind {
			t.Errorf("built provider Kind() = %q, want %q", p.Kind(), tc.kind)
		}
		if p.ID() != "p" {
			t.Errorf("built provider ID() = %q, want %q", p.ID(), "p")
		}
	}
}

func TestBuildPropagatesAPIKeyError(t *testing.T) {
	cfg := config.ProviderConfig{ID: "p", Kind: "anthropic", APIKeyEnv: "FACTORY_TEST_KEY_UNSET_XYZ"}
	_, err := Build(cfg, nil)
	if err == nil {
		t.Fatal("expected an error for a required, unset API key env var")
	}
}

func TestBuildPropagatesCapabilities(t *testing.T) {
	t.Setenv("FACTORY_TEST_KEY", "sk-test")
	cfg := config.ProviderConfig{ID: "p", Kind: "anthropic", APIKeyEnv: "FACTORY_TEST_KEY", SupportsImages: true, SupportsTools: true}
	p, err := Build(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.SupportsImages() || !p.SupportsTools() {
		t.Errorf("SupportsImages()=%v SupportsTools()=%v, want both true", p.SupportsImages(), p.SupportsTools())
	}
}

func TestBuildOAuthRequiresResolver(t *testing.T) {
	cfg := config.ProviderConfig{ID: "p", Kind: "openai-responses", AuthMode: "oauth"}
	_, err := Build(cfg, nil)
	if err == nil {
		t.Fatal("expected an error when auth_mode is oauth but resolve is nil")
	}
}

func TestBuildOAuthOnlySupportsOpenAIResponses(t *testing.T) {
	resolve := func(context.Context) (string, error) { return "tok", nil }
	cfg := config.ProviderConfig{ID: "p", Kind: "anthropic", AuthMode: "oauth"}
	_, err := Build(cfg, resolve)
	if err == nil {
		t.Fatal("expected an error: anthropic has no oauth-based adapter")
	}

	cfg2 := config.ProviderConfig{ID: "p", Kind: "openai-responses", AuthMode: "oauth"}
	p, err := Build(cfg2, resolve)
	if err != nil {
		t.Fatalf("openai-responses with a resolver should build cleanly: %v", err)
	}
	if p.Kind() != "openai-responses" {
		t.Errorf("Kind() = %q, want openai-responses", p.Kind())
	}
}
